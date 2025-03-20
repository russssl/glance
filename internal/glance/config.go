package glance

import (
	"bytes"
	"fmt"
	"html/template"
	"log"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"gopkg.in/yaml.v3"
)

const CONFIG_INCLUDE_RECURSION_DEPTH_LIMIT = 20

const (
	configVarTypeEnv         = "env"
	configVarTypeSecret      = "secret"
	configVarTypeFileFromEnv = "readFileFromEnv"
)

type config struct {
	Server struct {
		Host       string    `yaml:"host"`
		Port       uint16    `yaml:"port"`
		AssetsPath string    `yaml:"assets-path"`
		BaseURL    string    `yaml:"base-url"`
		StartedAt  time.Time `yaml:"-"` // used in custom css file
	} `yaml:"server"`

	Document struct {
		Head template.HTML `yaml:"head"`
	} `yaml:"document"`

	Theme struct {
		BackgroundColor          *hslColorField `yaml:"background-color"`
		PrimaryColor             *hslColorField `yaml:"primary-color"`
		PositiveColor            *hslColorField `yaml:"positive-color"`
		NegativeColor            *hslColorField `yaml:"negative-color"`
		Light                    bool           `yaml:"light"`
		ContrastMultiplier       float32        `yaml:"contrast-multiplier"`
		TextSaturationMultiplier float32        `yaml:"text-saturation-multiplier"`
		CustomCSSFile            string         `yaml:"custom-css-file"`
	} `yaml:"theme"`

	Branding struct {
		HideFooter   bool          `yaml:"hide-footer"`
		CustomFooter template.HTML `yaml:"custom-footer"`
		LogoText     string        `yaml:"logo-text"`
		LogoURL      string        `yaml:"logo-url"`
		FaviconURL   string        `yaml:"favicon-url"`
	} `yaml:"branding"`

	Pages []page `yaml:"pages"`
}

type page struct {
	Title                      string `yaml:"name"`
	Slug                       string `yaml:"slug"`
	Width                      string `yaml:"width"`
	ShowMobileHeader           bool   `yaml:"show-mobile-header"`
	ExpandMobilePageNavigation bool   `yaml:"expand-mobile-page-navigation"`
	HideDesktopNavigation      bool   `yaml:"hide-desktop-navigation"`
	CenterVertically           bool   `yaml:"center-vertically"`
	Columns                    []struct {
		Size    string  `yaml:"size"`
		Widgets widgets `yaml:"widgets"`
	} `yaml:"columns"`
	PrimaryColumnIndex int8       `yaml:"-"`
	mu                 sync.Mutex `yaml:"-"`
}

func newConfigFromYAML(contents []byte) (*config, error) {
	contents, err := parseConfigVariables(contents)
	if err != nil {
		return nil, err
	}

	config := &config{}
	config.Server.Port = 8080

	err = yaml.Unmarshal(contents, config)
	if err != nil {
		return nil, err
	}

	if err = isConfigStateValid(config); err != nil {
		return nil, err
	}

	for p := range config.Pages {
		for c := range config.Pages[p].Columns {
			for w := range config.Pages[p].Columns[c].Widgets {
				if err := config.Pages[p].Columns[c].Widgets[w].initialize(); err != nil {
					return nil, formatWidgetInitError(err, config.Pages[p].Columns[c].Widgets[w])
				}
			}
		}
	}

	return config, nil
}

var configVariablePattern = regexp.MustCompile(`(^|.)\$\{(?:([a-zA-Z]+):)?([a-zA-Z0-9_-]+)\}`)

// Parses variables defined in the config such as:
// ${API_KEY} 				            - gets replaced with the value of the API_KEY environment variable
// \${API_KEY} 					        - escaped, gets used as is without the \ in the config
// ${secret:api_key} 			        - value gets loaded from /run/secrets/api_key
// ${readFileFromEnv:PATH_TO_SECRET}    - value gets loaded from the file path specified in the environment variable PATH_TO_SECRET
//
// TODO: don't match against commented out sections, not sure exactly how since
// variables can be placed anywhere and used to modify the YAML structure itself
func parseConfigVariables(contents []byte) ([]byte, error) {
	var err error

	replaced := configVariablePattern.ReplaceAllFunc(contents, func(match []byte) []byte {
		if err != nil {
			return nil
		}

		groups := configVariablePattern.FindSubmatch(match)
		if len(groups) != 4 {
			// we can't handle this match, this shouldn't happen unless the number of groups
			// in the regex has been changed without updating the below code
			return match
		}

		typeAsString := string(groups[2])
		variableType := ternary(typeAsString == "", configVarTypeEnv, typeAsString)
		value := string(groups[3])

		prefix := string(groups[1])
		if prefix == `\` {
			if len(match) >= 2 {
				return match[1:]
			} else {
				return nil
			}
		}

		parsedValue, localErr := parseConfigVariableOfType(variableType, value)
		if localErr != nil {
			err = fmt.Errorf("parsing variable: %v", localErr)
			return nil
		}

		return []byte(prefix + parsedValue)
	})

	if err != nil {
		return nil, err
	}

	return replaced, nil
}

func parseConfigVariableOfType(variableType, value string) (string, error) {
	switch variableType {
	case configVarTypeEnv:
		v, found := os.LookupEnv(value)
		if !found {
			return "", fmt.Errorf("environment variable %s not found", value)
		}

		return v, nil
	case configVarTypeSecret:
		secretPath := filepath.Join("/run/secrets", value)
		secret, err := os.ReadFile(secretPath)
		if err != nil {
			return "", fmt.Errorf("reading secret file: %v", err)
		}

		return strings.TrimSpace(string(secret)), nil
	case configVarTypeFileFromEnv:
		filePath, found := os.LookupEnv(value)
		if !found {
			return "", fmt.Errorf("readFileFromEnv: environment variable %s not found", value)
		}

		fileContents, err := os.ReadFile(filePath)
		if err != nil {
			return "", fmt.Errorf("readFileFromEnv: reading file from %s: %v", value, err)
		}

		return strings.TrimSpace(string(fileContents)), nil
	default:
		return "", fmt.Errorf("unknown variable type %s with value %s", variableType, value)
	}
}

func formatWidgetInitError(err error, w widget) error {
	return fmt.Errorf("%s widget: %v", w.GetType(), err)
}

var configIncludePattern = regexp.MustCompile(`(?m)^([ \t]*)(?:-[ \t]*)?(?:!|\$)include:[ \t]*(.+)$`)

func parseYAMLIncludes(mainFilePath string) ([]byte, map[string]struct{}, error) {
	return recursiveParseYAMLIncludes(mainFilePath, nil, 0)
}

func recursiveParseYAMLIncludes(mainFilePath string, includes map[string]struct{}, depth int) ([]byte, map[string]struct{}, error) {
	if depth > CONFIG_INCLUDE_RECURSION_DEPTH_LIMIT {
		return nil, nil, fmt.Errorf("recursion depth limit of %d reached", CONFIG_INCLUDE_RECURSION_DEPTH_LIMIT)
	}

	mainFileContents, err := os.ReadFile(mainFilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("reading %s: %w", mainFilePath, err)
	}

	mainFileAbsPath, err := filepath.Abs(mainFilePath)
	if err != nil {
		return nil, nil, fmt.Errorf("getting absolute path of %s: %w", mainFilePath, err)
	}
	mainFileDir := filepath.Dir(mainFileAbsPath)

	if includes == nil {
		includes = make(map[string]struct{})
	}
	var includesLastErr error

	mainFileContents = configIncludePattern.ReplaceAllFunc(mainFileContents, func(match []byte) []byte {
		if includesLastErr != nil {
			return nil
		}

		matches := configIncludePattern.FindSubmatch(match)
		if len(matches) != 3 {
			includesLastErr = fmt.Errorf("invalid include match: %v", matches)
			return nil
		}

		indent := string(matches[1])
		includeFilePath := strings.TrimSpace(string(matches[2]))
		if !filepath.IsAbs(includeFilePath) {
			includeFilePath = filepath.Join(mainFileDir, includeFilePath)
		}

		var fileContents []byte
		var err error

		includes[includeFilePath] = struct{}{}

		fileContents, includes, err = recursiveParseYAMLIncludes(includeFilePath, includes, depth+1)
		if err != nil {
			includesLastErr = err
			return nil
		}

		return []byte(prefixStringLines(indent, string(fileContents)))
	})

	if includesLastErr != nil {
		return nil, nil, includesLastErr
	}

	return mainFileContents, includes, nil
}

func configFilesWatcher(
	mainFilePath string,
	lastContents []byte,
	lastIncludes map[string]struct{},
	onChange func(newContents []byte),
	onErr func(error),
) (func() error, error) {
	mainFileAbsPath, err := filepath.Abs(mainFilePath)
	if err != nil {
		return nil, fmt.Errorf("getting absolute path of main file: %w", err)
	}

	// TODO: refactor, flaky
	lastIncludes[mainFileAbsPath] = struct{}{}

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		return nil, fmt.Errorf("creating watcher: %w", err)
	}

	updateWatchedFiles := func(previousWatched map[string]struct{}, newWatched map[string]struct{}) {
		for filePath := range previousWatched {
			if _, ok := newWatched[filePath]; !ok {
				watcher.Remove(filePath)
			}
		}

		for filePath := range newWatched {
			if _, ok := previousWatched[filePath]; !ok {
				if err := watcher.Add(filePath); err != nil {
					log.Printf(
						"Could not add file to watcher, changes to this file will not trigger a reload. path: %s, error: %v",
						filePath, err,
					)
				}
			}
		}
	}

	updateWatchedFiles(nil, lastIncludes)

	// needed for lastContents and lastIncludes because they get updated in multiple goroutines
	mu := sync.Mutex{}

	parseAndCompareBeforeCallback := func() {
		currentContents, currentIncludes, err := parseYAMLIncludes(mainFilePath)
		if err != nil {
			onErr(fmt.Errorf("parsing main file contents for comparison: %w", err))
			return
		}

		// TODO: refactor, flaky
		currentIncludes[mainFileAbsPath] = struct{}{}

		mu.Lock()
		defer mu.Unlock()

		if !maps.Equal(currentIncludes, lastIncludes) {
			updateWatchedFiles(lastIncludes, currentIncludes)
			lastIncludes = currentIncludes
		}

		if !bytes.Equal(lastContents, currentContents) {
			lastContents = currentContents
			onChange(currentContents)
		}
	}

	const debounceDuration = 500 * time.Millisecond
	var debounceTimer *time.Timer
	debouncedParseAndCompareBeforeCallback := func() {
		if debounceTimer != nil {
			debounceTimer.Stop()
			debounceTimer.Reset(debounceDuration)
		} else {
			debounceTimer = time.AfterFunc(debounceDuration, parseAndCompareBeforeCallback)
		}
	}

	deleteLastInclude := func(filePath string) {
		mu.Lock()
		defer mu.Unlock()
		fileAbsPath, _ := filepath.Abs(filePath)
		delete(lastIncludes, fileAbsPath)
	}

	go func() {
		for {
			select {
			case event, isOpen := <-watcher.Events:
				if !isOpen {
					return
				}
				if event.Has(fsnotify.Write) {
					debouncedParseAndCompareBeforeCallback()
				} else if event.Has(fsnotify.Rename) {
					// on linux the file will no longer be watched after a rename, on windows
					// it will continue to be watched with the new name but we have no access to
					// the new name in this event in order to stop watching it manually and match the
					// behavior in linux, may lead to weird unintended behaviors on windows as we're
					// only handling renames from linux's perspective
					// see https://github.com/fsnotify/fsnotify/issues/255

					// remove the old file from our manually tracked includes, calling
					// debouncedParseAndCompareBeforeCallback will re-add it if it's still
					// required after it triggers
					deleteLastInclude(event.Name)

					// wait for file to maybe get created again
					// see https://github.com/glanceapp/glance/pull/358
					for range 10 {
						if _, err := os.Stat(event.Name); err == nil {
							break
						}
						time.Sleep(200 * time.Millisecond)
					}

					debouncedParseAndCompareBeforeCallback()
				} else if event.Has(fsnotify.Remove) {
					deleteLastInclude(event.Name)
					debouncedParseAndCompareBeforeCallback()
				}
			case err, isOpen := <-watcher.Errors:
				if !isOpen {
					return
				}
				onErr(fmt.Errorf("watcher error: %w", err))
			}
		}
	}()

	onChange(lastContents)

	return func() error {
		if debounceTimer != nil {
			debounceTimer.Stop()
		}

		return watcher.Close()
	}, nil
}

func isConfigStateValid(config *config) error {
	if len(config.Pages) == 0 {
		return fmt.Errorf("no pages configured")
	}

	if config.Server.AssetsPath != "" {
		if _, err := os.Stat(config.Server.AssetsPath); os.IsNotExist(err) {
			return fmt.Errorf("assets directory does not exist: %s", config.Server.AssetsPath)
		}
	}

	for i := range config.Pages {
		if config.Pages[i].Title == "" {
			return fmt.Errorf("page %d has no name", i+1)
		}

		if config.Pages[i].Width != "" && (config.Pages[i].Width != "wide" && config.Pages[i].Width != "slim") {
			return fmt.Errorf("page %d: width can only be either wide or slim", i+1)
		}

		if len(config.Pages[i].Columns) == 0 {
			return fmt.Errorf("page %d has no columns", i+1)
		}

		if config.Pages[i].Width == "slim" {
			if len(config.Pages[i].Columns) > 2 {
				return fmt.Errorf("page %d is slim and cannot have more than 2 columns", i+1)
			}
		} else {
			if len(config.Pages[i].Columns) > 3 {
				return fmt.Errorf("page %d has more than 3 columns", i+1)
			}
		}

		columnSizesCount := make(map[string]int)

		for j := range config.Pages[i].Columns {
			if config.Pages[i].Columns[j].Size != "small" && config.Pages[i].Columns[j].Size != "full" {
				return fmt.Errorf("column %d of page %d: size can only be either small or full", j+1, i+1)
			}

			columnSizesCount[config.Pages[i].Columns[j].Size]++
		}

		full := columnSizesCount["full"]

		if full > 2 || full == 0 {
			return fmt.Errorf("page %d must have either 1 or 2 full width columns", i+1)
		}
	}

	return nil
}
