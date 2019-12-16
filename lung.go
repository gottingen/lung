package lung

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	yaml "gopkg.in/yaml.v2"

	"github.com/fsnotify/fsnotify"
	"github.com/gottingen/felix"
	"github.com/gottingen/felix/vfs"
	"github.com/gottingen/gekko/cast"
	"github.com/gottingen/gekko/env"
	"github.com/gottingen/gekko/gflag"
	"github.com/gottingen/viper"
	"github.com/magiconair/properties"
	"github.com/mitchellh/mapstructure"
	toml "github.com/pelletier/go-toml"
)

// ConfigMarshalError happens when failing to marshal the configuration.
type ConfigMarshalError struct {
	err error
}

// Error returns the formatted configuration error.
func (e ConfigMarshalError) Error() string {
	return fmt.Sprintf("While marshaling config: %s", e.err.Error())
}

var l *Lung

func init() {
	l = New()
}

// UnsupportedConfigError denotes encountering an unsupported
// configuration filetype.
type UnsupportedConfigError string

// Error returns the formatted configuration error.
func (str UnsupportedConfigError) Error() string {
	return fmt.Sprintf("Unsupported Config Type %q", string(str))
}

// ConfigFileNotFoundError denotes failing to find configuration file.
type ConfigFileNotFoundError struct {
	name, locations string
}

// Error returns the formatted configuration error.
func (fnfe ConfigFileNotFoundError) Error() string {
	return fmt.Sprintf("Config File %q Not Found in %q", fnfe.name, fnfe.locations)
}

// A DecoderConfigOption can be passed to lung.Unmarshal to configure
// mapstructure.DecoderConfig options
type DecoderConfigOption func(*mapstructure.DecoderConfig)

// DecodeHook returns a DecoderConfigOption which overrides the default
// DecoderConfig.DecodeHook value, the default is:
//
//  mapstructure.ComposeDecodeHookFunc(
//		mapstructure.StringToTimeDurationHookFunc(),
//		mapstructure.StringToSliceHookFunc(","),
//	)
func DecodeHook(hook mapstructure.DecodeHookFunc) DecoderConfigOption {
	return func(c *mapstructure.DecoderConfig) {
		c.DecodeHook = hook
	}
}

// Lung is a prioritized configuration registry. It
// maintains a set of configuration sources, fetches
// values to populate those, and provides them according
// to the source's priority.
// The priority of the sources is the following:
// 1. overrides
// 2. flags
// 3. env. variables
// 4. config file
// 5. key/value store
// 6. defaults
//
// For example, if values from the following sources were loaded:
//
//  Defaults : {
//  	"secret": "",
//  	"user": "default",
//  	"endpoint": "https://localhost"
//  }
//  Config : {
//  	"user": "root"
//  	"secret": "defaultsecret"
//  }
//  Env : {
//  	"secret": "somesecretkey"
//  }
//
// The resulting config will have the following values:
//
//	{
//		"secret": "somesecretkey",
//		"user": "root",
//		"endpoint": "https://localhost"
//	}
type Lung struct {
	// Delimiter that separates a list of keys
	// used to access a nested value in one go
	keyDelim string

	// A set of paths to look for the config file in
	configPaths []string

	// The filesystem to read config from.
	fs felix.Felix

	// Name of file to look for inside the path
	configName        string
	configFile        string
	configType        string
	configPermissions os.FileMode
	envPrefix         string

	automaticEnvApplied bool
	envKeyReplacer      *strings.Replacer
	allowEmptyEnv       bool

	config         map[string]interface{}
	override       map[string]interface{}
	defaults       map[string]interface{}
	kvstore        map[string]interface{}
	gflags         map[string]FlagValue
	env            map[string]string
	aliases        map[string]string
	typeByDefValue bool

	// Store read properties on the object so that we can write back in order with comments.
	// This will only be used if the configuration read is a properties file.
	properties *properties.Properties

	onConfigChange func(fsnotify.Event)

	logger *viper.Logger
}

// New returns an initialized Lung instance.
func New() *Lung {
	l := new(Lung)
	l.keyDelim = "."
	l.configName = "config"
	l.configPermissions = os.FileMode(0644)
	l.fs = felix.NewOsVfs()
	l.config = make(map[string]interface{})
	l.override = make(map[string]interface{})
	l.defaults = make(map[string]interface{})
	l.kvstore = make(map[string]interface{})
	l.gflags = make(map[string]FlagValue)
	l.env = make(map[string]string)
	l.aliases = make(map[string]string)
	l.typeByDefValue = false
	l.logger = viper.NewExample()

	return l
}

// Reset is intended for testing, will reset all to default settings.
// In the public interface for the lung package so applications
// can use it in their testing as well.
func Reset() {
	l = New()
	SupportedExts = []string{"json", "toml", "yaml", "yml", "properties", "props", "prop", "dotenv", "env"}
}

// SupportedExts are universally supported extensions.
var SupportedExts = []string{"json", "toml", "yaml", "yml", "properties", "props", "prop", "dotenv", "env"}

func OnConfigChange(run func(in fsnotify.Event)) { l.OnConfigChange(run) }
func (l *Lung) OnConfigChange(run func(in fsnotify.Event)) {
	l.onConfigChange = run
}

func WatchConfig() { l.WatchConfig() }

func (l *Lung) WatchConfig() {
	initWG := sync.WaitGroup{}
	initWG.Add(1)
	go func() {
		watcher, err := fsnotify.NewWatcher()
		if err != nil {
			l.logger.Error(err.Error())
		}
		defer watcher.Close()
		// we have to watch the entire directory to pick up renames/atomic saves in a cross-platform way
		filename, err := l.getConfigFile()
		if err != nil {
			l.logger.Info("err:", viper.Error(err))
			initWG.Done()
			return
		}

		configFile := filepath.Clean(filename)
		configDir, _ := filepath.Split(configFile)
		realConfigFile, _ := filepath.EvalSymlinks(filename)

		eventsWG := sync.WaitGroup{}
		eventsWG.Add(1)
		go func() {
			for {
				select {
				case event, ok := <-watcher.Events:
					if !ok { // 'Events' channel is closed
						eventsWG.Done()
						return
					}
					currentConfigFile, _ := filepath.EvalSymlinks(filename)
					// we only care about the config file with the following cases:
					// 1 - if the config file was modified or created
					// 2 - if the real path to the config file changed (eg: k8s ConfigMap replacement)
					const writeOrCreateMask = fsnotify.Write | fsnotify.Create
					if (filepath.Clean(event.Name) == configFile &&
						event.Op&writeOrCreateMask != 0) ||
						(currentConfigFile != "" && currentConfigFile != realConfigFile) {
						realConfigFile = currentConfigFile
						err := l.ReadInConfig()
						if err != nil {
							l.logger.Info("error reading config file ", viper.Error(err))
						}
						if l.onConfigChange != nil {
							l.onConfigChange(event)
						}
					} else if filepath.Clean(event.Name) == configFile &&
						event.Op&fsnotify.Remove&fsnotify.Remove != 0 {
						eventsWG.Done()
						return
					}

				case err, ok := <-watcher.Errors:
					if ok { // 'Errors' channel is not closed
						l.logger.Warn("watcher error: ", viper.Error(err))
					}
					eventsWG.Done()
					return
				}
			}
		}()
		watcher.Add(configDir)
		initWG.Done()   // done initializing the watch in this go routine, so the parent routine can move on...
		eventsWG.Wait() // now, wait for event loop to end in this go-routine...
	}()
	initWG.Wait() // make sure that the go routine above fully ended before returning
}

// SetConfigFile explicitly defines the path, name and extension of the config file.
// Lung will use this and not check any of the config paths.
func SetConfigFile(in string) { l.SetConfigFile(in) }
func (l *Lung) SetConfigFile(in string) {
	if in != "" {
		l.configFile = in
	}
}

// SetEnvPrefix defines a prefix that ENVIRONMENT variables will use.
// E.g. if your prefix is "spf", the env registry will look for env
// variables that start with "SPF_".
func SetEnvPrefix(in string) { l.SetEnvPrefix(in) }
func (l *Lung) SetEnvPrefix(in string) {
	if in != "" {
		l.envPrefix = in
	}
}

func (l *Lung) mergeWithEnvPrefix(in string) string {
	if l.envPrefix != "" {
		return strings.ToUpper(l.envPrefix + "_" + in)
	}

	return strings.ToUpper(in)
}

// AllowEmptyEnv tells Lung to consider set,
// but empty environment variables as valid values instead of falling back.
// For backward compatibility reasons this is false by default.
func AllowEmptyEnv(allowEmptyEnv bool) { l.AllowEmptyEnv(allowEmptyEnv) }
func (l *Lung) AllowEmptyEnv(allowEmptyEnv bool) {
	l.allowEmptyEnv = allowEmptyEnv
}

// TODO: should getEnv logic be moved into find(). Can generalize the use of
// rewriting keys many things, Ex: Get('someKey') -> some_key
// (camel case to snake case for JSON keys perhaps)

// getEnv is a wrapper around os.Getenv which replaces characters in the original
// key. This allows env vars which have different keys than the config object
// keys.
func (l *Lung) getEnv(key string) (string, bool) {
	if l.envKeyReplacer != nil {
		key = l.envKeyReplacer.Replace(key)
	}

	val, ok := os.LookupEnv(key)

	return val, ok && (l.allowEmptyEnv || val != "")
}

// ConfigFileUsed returns the file used to populate the config registry.
func ConfigFileUsed() string           { return l.ConfigFileUsed() }
func (l *Lung) ConfigFileUsed() string { return l.configFile }

// AddConfigPath adds a path for Lung to search for the config file in.
// Can be called multiple times to define multiple search paths.
func AddConfigPath(in string) { l.AddConfigPath(in) }
func (l *Lung) AddConfigPath(in string) {
	if in != "" {
		absin := absPathify(in)
		l.logger.Info("adding %s to paths to search", viper.String("absin", absin))
		if !stringInSlice(absin, l.configPaths) {
			l.configPaths = append(l.configPaths, absin)
		}
	}
}

// searchMap recursively searches for a value for path in source map.
// Returns nil if not found.
// Note: This assumes that the path entries and map keys are lower cased.
func (l *Lung) searchMap(source map[string]interface{}, path []string) interface{} {
	if len(path) == 0 {
		return source
	}

	next, ok := source[path[0]]
	if ok {
		// Fast path
		if len(path) == 1 {
			return next
		}

		// Nested case
		switch next.(type) {
		case map[interface{}]interface{}:
			return l.searchMap(cast.ToStringMap(next), path[1:])
		case map[string]interface{}:
			// Type assertion is safe here since it is only reached
			// if the type of `next` is the same as the type being asserted
			return l.searchMap(next.(map[string]interface{}), path[1:])
		default:
			// got a value but nested key expected, return "nil" for not found
			return nil
		}
	}
	return nil
}

// searchMapWithPathPrefixes recursively searches for a value for path in source map.
//
// While searchMap() considers each path element as a single map key, this
// function searches for, and prioritizes, merged path elements.
// e.g., if in the source, "foo" is defined with a sub-key "bar", and "foo.bar"
// is also defined, this latter value is returned for path ["foo", "bar"].
//
// This should be useful only at config level (other maps may not contain dots
// in their keys).
//
// Note: This assumes that the path entries and map keys are lower cased.
func (l *Lung) searchMapWithPathPrefixes(source map[string]interface{}, path []string) interface{} {
	if len(path) == 0 {
		return source
	}

	// search for path prefixes, starting from the longest one
	for i := len(path); i > 0; i-- {
		prefixKey := strings.ToLower(strings.Join(path[0:i], l.keyDelim))

		next, ok := source[prefixKey]
		if ok {
			// Fast path
			if i == len(path) {
				return next
			}

			// Nested case
			var val interface{}
			switch next.(type) {
			case map[interface{}]interface{}:
				val = l.searchMapWithPathPrefixes(cast.ToStringMap(next), path[i:])
			case map[string]interface{}:
				// Type assertion is safe here since it is only reached
				// if the type of `next` is the same as the type being asserted
				val = l.searchMapWithPathPrefixes(next.(map[string]interface{}), path[i:])
			default:
				// got a value but nested key expected, do nothing and look for next prefix
			}
			if val != nil {
				return val
			}
		}
	}

	// not found
	return nil
}

// isPathShadowedInDeepMap makes sure the given path is not shadowed somewhere
// on its path in the map.
// e.g., if "foo.bar" has a value in the given map, it “shadows”
//       "foo.bar.baz" in a lower-priority map
func (l *Lung) isPathShadowedInDeepMap(path []string, m map[string]interface{}) string {
	var parentVal interface{}
	for i := 1; i < len(path); i++ {
		parentVal = l.searchMap(m, path[0:i])
		if parentVal == nil {
			// not found, no need to add more path elements
			return ""
		}
		switch parentVal.(type) {
		case map[interface{}]interface{}:
			continue
		case map[string]interface{}:
			continue
		default:
			// parentVal is a regular value which shadows "path"
			return strings.Join(path[0:i], l.keyDelim)
		}
	}
	return ""
}

// isPathShadowedInFlatMap makes sure the given path is not shadowed somewhere
// in a sub-path of the map.
// e.g., if "foo.bar" has a value in the given map, it “shadows”
//       "foo.bar.baz" in a lower-priority map
func (l *Lung) isPathShadowedInFlatMap(path []string, mi interface{}) string {
	// unify input map
	var m map[string]interface{}
	switch mi.(type) {
	case map[string]string, map[string]FlagValue:
		m = cast.ToStringMap(mi)
	default:
		return ""
	}

	// scan paths
	var parentKey string
	for i := 1; i < len(path); i++ {
		parentKey = strings.Join(path[0:i], l.keyDelim)
		if _, ok := m[parentKey]; ok {
			return parentKey
		}
	}
	return ""
}

// isPathShadowedInAutoEnv makes sure the given path is not shadowed somewhere
// in the environment, when automatic env is on.
// e.g., if "foo.bar" has a value in the environment, it “shadows”
//       "foo.bar.baz" in a lower-priority map
func (l *Lung) isPathShadowedInAutoEnv(path []string) string {
	var parentKey string
	for i := 1; i < len(path); i++ {
		parentKey = strings.Join(path[0:i], l.keyDelim)
		if _, ok := l.getEnv(l.mergeWithEnvPrefix(parentKey)); ok {
			return parentKey
		}
	}
	return ""
}

// SetTypeByDefaultValue enables or disables the inference of a key value's
// type when the Get function is used based upon a key's default value as
// opposed to the value returned based on the normal fetch logic.
//
// For example, if a key has a default value of []string{} and the same key
// is set via an environment variable to "a b c", a call to the Get function
// would return a string slice for the key if the key's type is inferred by
// the default value and the Get function would return:
//
//   []string {"a", "b", "c"}
//
// Otherwise the Get function would return:
//
//   "a b c"
func SetTypeByDefaultValue(enable bool) { l.SetTypeByDefaultValue(enable) }
func (l *Lung) SetTypeByDefaultValue(enable bool) {
	l.typeByDefValue = enable
}

// GetLung gets the global Lung instance.
func GetLung() *Lung {
	return l
}

// Get can retrieve any value given the key to use.
// Get is case-insensitive for a key.
// Get has the behavior of returning the value associated with the first
// place from where it is set. Lung will check in the following order:
// override, flag, env, config file, key/value store, default
//
// Get returns an interface. For a specific value use one of the Get____ methods.
func Get(key string) interface{} { return l.Get(key) }
func (l *Lung) Get(key string) interface{} {
	lcaseKey := strings.ToLower(key)
	val := l.find(lcaseKey)
	if val == nil {
		return nil
	}

	if l.typeByDefValue {
		// TODO(bep) this branch isn't covered by a single test.
		valType := val
		path := strings.Split(lcaseKey, l.keyDelim)
		defVal := l.searchMap(l.defaults, path)
		if defVal != nil {
			valType = defVal
		}

		switch valType.(type) {
		case bool:
			return cast.ToBool(val)
		case string:
			return cast.ToString(val)
		case int32, int16, int8, int:
			return cast.ToInt(val)
		case uint:
			return cast.ToUint(val)
		case uint32:
			return cast.ToUint32(val)
		case uint64:
			return cast.ToUint64(val)
		case int64:
			return cast.ToInt64(val)
		case float64, float32:
			return cast.ToFloat64(val)
		case time.Time:
			return cast.ToTime(val)
		case time.Duration:
			return cast.ToDuration(val)
		case []string:
			return cast.ToStringSlice(val)
		case []int:
			return cast.ToIntSlice(val)
		}
	}

	return val
}

// Sub returns new Lung instance representing a sub tree of this instance.
// Sub is case-insensitive for a key.
func Sub(key string) *Lung { return l.Sub(key) }
func (l *Lung) Sub(key string) *Lung {
	subl := New()
	data := l.Get(key)
	if data == nil {
		return nil
	}

	if reflect.TypeOf(data).Kind() == reflect.Map {
		subl.config = cast.ToStringMap(data)
		return subl
	}
	return nil
}

// GetString returns the value associated with the key as a string.
func GetString(key string) string { return l.GetString(key) }
func (l *Lung) GetString(key string) string {
	return cast.ToString(l.Get(key))
}

// GetBool returns the value associated with the key as a boolean.
func GetBool(key string) bool { return l.GetBool(key) }
func (l *Lung) GetBool(key string) bool {
	return cast.ToBool(l.Get(key))
}

// GetInt returns the value associated with the key as an integer.
func GetInt(key string) int { return l.GetInt(key) }
func (l *Lung) GetInt(key string) int {
	return cast.ToInt(l.Get(key))
}

// GetInt32 returns the value associated with the key as an integer.
func GetInt32(key string) int32 { return l.GetInt32(key) }
func (l *Lung) GetInt32(key string) int32 {
	return cast.ToInt32(l.Get(key))
}

// GetInt64 returns the value associated with the key as an integer.
func GetInt64(key string) int64 { return l.GetInt64(key) }
func (l *Lung) GetInt64(key string) int64 {
	return cast.ToInt64(l.Get(key))
}

// GetUint returns the value associated with the key as an unsigned integer.
func GetUint(key string) uint { return l.GetUint(key) }
func (l *Lung) GetUint(key string) uint {
	return cast.ToUint(l.Get(key))
}

// GetUint32 returns the value associated with the key as an unsigned integer.
func GetUint32(key string) uint32 { return l.GetUint32(key) }
func (l *Lung) GetUint32(key string) uint32 {
	return cast.ToUint32(l.Get(key))
}

// GetUint64 returns the value associated with the key as an unsigned integer.
func GetUint64(key string) uint64 { return l.GetUint64(key) }
func (l *Lung) GetUint64(key string) uint64 {
	return cast.ToUint64(l.Get(key))
}

// GetFloat64 returns the value associated with the key as a float64.
func GetFloat64(key string) float64 { return l.GetFloat64(key) }
func (l *Lung) GetFloat64(key string) float64 {
	return cast.ToFloat64(l.Get(key))
}

// GetTime returns the value associated with the key as time.
func GetTime(key string) time.Time { return l.GetTime(key) }
func (l *Lung) GetTime(key string) time.Time {
	return cast.ToTime(l.Get(key))
}

// GetDuration returns the value associated with the key as a duration.
func GetDuration(key string) time.Duration { return l.GetDuration(key) }
func (l *Lung) GetDuration(key string) time.Duration {
	return cast.ToDuration(l.Get(key))
}

// GetIntSlice returns the value associated with the key as a slice of int values.
func GetIntSlice(key string) []int { return l.GetIntSlice(key) }
func (l *Lung) GetIntSlice(key string) []int {
	return cast.ToIntSlice(l.Get(key))
}

// GetStringSlice returns the value associated with the key as a slice of strings.
func GetStringSlice(key string) []string { return l.GetStringSlice(key) }
func (l *Lung) GetStringSlice(key string) []string {
	return cast.ToStringSlice(l.Get(key))
}

// GetStringMap returns the value associated with the key as a map of interfaces.
func GetStringMap(key string) map[string]interface{} { return l.GetStringMap(key) }
func (l *Lung) GetStringMap(key string) map[string]interface{} {
	return cast.ToStringMap(l.Get(key))
}

// GetStringMapString returns the value associated with the key as a map of strings.
func GetStringMapString(key string) map[string]string { return l.GetStringMapString(key) }
func (l *Lung) GetStringMapString(key string) map[string]string {
	return cast.ToStringMapString(l.Get(key))
}

// GetStringMapStringSlice returns the value associated with the key as a map to a slice of strings.
func GetStringMapStringSlice(key string) map[string][]string { return l.GetStringMapStringSlice(key) }
func (l *Lung) GetStringMapStringSlice(key string) map[string][]string {
	return cast.ToStringMapStringSlice(l.Get(key))
}

// GetSizeInBytes returns the size of the value associated with the given key
// in bytes.
func GetSizeInBytes(key string) uint { return l.GetSizeInBytes(key) }
func (l *Lung) GetSizeInBytes(key string) uint {
	sizeStr := cast.ToString(l.Get(key))
	return parseSizeInBytes(sizeStr)
}

// UnmarshalKey takes a single key and unmarshals it into a Struct.
func UnmarshalKey(key string, rawVal interface{}, opts ...DecoderConfigOption) error {
	return l.UnmarshalKey(key, rawVal, opts...)
}
func (l *Lung) UnmarshalKey(key string, rawVal interface{}, opts ...DecoderConfigOption) error {
	err := decode(l.Get(key), defaultDecoderConfig(rawVal, opts...))

	if err != nil {
		return err
	}

	return nil
}

// Unmarshal unmarshals the config into a Struct. Make sure that the tags
// on the fields of the structure are properly set.
func Unmarshal(rawVal interface{}, opts ...DecoderConfigOption) error {
	return l.Unmarshal(rawVal, opts...)
}
func (l *Lung) Unmarshal(rawVal interface{}, opts ...DecoderConfigOption) error {
	err := decode(l.AllSettings(), defaultDecoderConfig(rawVal, opts...))

	if err != nil {
		return err
	}

	return nil
}

// defaultDecoderConfig returns default mapsstructure.DecoderConfig with suppot
// of time.Duration values & string slices
func defaultDecoderConfig(output interface{}, opts ...DecoderConfigOption) *mapstructure.DecoderConfig {
	c := &mapstructure.DecoderConfig{
		Metadata:         nil,
		Result:           output,
		WeaklyTypedInput: true,
		DecodeHook: mapstructure.ComposeDecodeHookFunc(
			mapstructure.StringToTimeDurationHookFunc(),
			mapstructure.StringToSliceHookFunc(","),
		),
	}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

// A wrapper around mapstructure.Decode that mimics the WeakDecode functionality
func decode(input interface{}, config *mapstructure.DecoderConfig) error {
	decoder, err := mapstructure.NewDecoder(config)
	if err != nil {
		return err
	}
	return decoder.Decode(input)
}

// UnmarshalExact unmarshals the config into a Struct, erroring if a field is nonexistent
// in the destination struct.
func (l *Lung) UnmarshalExact(rawVal interface{}) error {
	config := defaultDecoderConfig(rawVal)
	config.ErrorUnused = true

	err := decode(l.AllSettings(), config)

	if err != nil {
		return err
	}

	return nil
}

// BindPFlags binds a full flag set to the configuration, using each flag's long
// name as the config key.
func BindPFlags(flags *gflag.FlagSet) error { return l.BindPFlags(flags) }
func (l *Lung) BindPFlags(flags *gflag.FlagSet) error {
	return l.BindFlagValues(gflagValueSet{flags})
}

// BindPFlag binds a specific key to a gflag (as used by cobra).
// Example (where serverCmd is a Cobra instance):
//
//	 serverCmd.Flags().Int("port", 1138, "Port to run Application server on")
//	 Lung.BindPFlag("port", serverCmd.Flags().Lookup("port"))
//
func BindPFlag(key string, flag *gflag.Flag) error { return l.BindPFlag(key, flag) }
func (l *Lung) BindPFlag(key string, flag *gflag.Flag) error {
	return l.BindFlagValue(key, gflagValue{flag})
}

// BindFlagValues binds a full FlagValue set to the configuration, using each flag's long
// name as the config key.
func BindFlagValues(flags FlagValueSet) error { return l.BindFlagValues(flags) }
func (l *Lung) BindFlagValues(flags FlagValueSet) (err error) {
	flags.VisitAll(func(flag FlagValue) {
		if err = l.BindFlagValue(flag.Name(), flag); err != nil {
			return
		}
	})
	return nil
}

// BindFlagValue binds a specific key to a FlagValue.
// Example (where serverCmd is a Cobra instance):
//
//	 serverCmd.Flags().Int("port", 1138, "Port to run Application server on")
//	 Lung.BindFlagValue("port", serverCmd.Flags().Lookup("port"))
//
func BindFlagValue(key string, flag FlagValue) error { return l.BindFlagValue(key, flag) }
func (l *Lung) BindFlagValue(key string, flag FlagValue) error {
	if flag == nil {
		return fmt.Errorf("flag for %q is nil", key)
	}
	l.gflags[strings.ToLower(key)] = flag
	return nil
}

// BindEnv binds a Lung key to a ENV variable.
// ENV variables are case sensitive.
// If only a key is provided, it will use the env key matching the key, uppercased.
// EnvPrefix will be used when set when env name is not provided.
func BindEnv(input ...string) error { return l.BindEnv(input...) }
func (l *Lung) BindEnv(input ...string) error {
	var key, envkey string
	if len(input) == 0 {
		return fmt.Errorf("BindEnv missing key to bind to")
	}

	key = strings.ToLower(input[0])

	if len(input) == 1 {
		envkey = l.mergeWithEnvPrefix(key)
	} else {
		envkey = input[1]
	}

	l.env[key] = envkey

	return nil
}

// Given a key, find the value.
// Lung will check in the following order:
// flag, env, config file, key/value store, default.
// Lung will check to see if an alias exists first.
// Note: this assumes a lower-cased key given.
func (l *Lung) find(lcaseKey string) interface{} {

	var (
		val    interface{}
		exists bool
		path   = strings.Split(lcaseKey, l.keyDelim)
		nested = len(path) > 1
	)

	// compute the path through the nested maps to the nested value
	if nested && l.isPathShadowedInDeepMap(path, castMapStringToMapInterface(l.aliases)) != "" {
		return nil
	}

	// if the requested key is an alias, then return the proper key
	lcaseKey = l.realKey(lcaseKey)
	path = strings.Split(lcaseKey, l.keyDelim)
	nested = len(path) > 1

	// Set() override first
	val = l.searchMap(l.override, path)
	if val != nil {
		return val
	}
	if nested && l.isPathShadowedInDeepMap(path, l.override) != "" {
		return nil
	}

	// PFlag override next
	flag, exists := l.gflags[lcaseKey]
	if exists && flag.HasChanged() {
		switch flag.ValueType() {
		case "int", "int8", "int16", "int32", "int64":
			return cast.ToInt(flag.ValueString())
		case "bool":
			return cast.ToBool(flag.ValueString())
		case "stringSlice":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			res, _ := readAsCSV(s)
			return res
		case "intSlice":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			res, _ := readAsCSV(s)
			return cast.ToIntSlice(res)
		default:
			return flag.ValueString()
		}
	}
	if nested && l.isPathShadowedInFlatMap(path, l.gflags) != "" {
		return nil
	}

	// Env override next
	if l.automaticEnvApplied {
		// even if it hasn't been registered, if automaticEnv is used,
		// check any Get request
		if val, ok := l.getEnv(l.mergeWithEnvPrefix(lcaseKey)); ok {
			return val
		}
		if nested && l.isPathShadowedInAutoEnv(path) != "" {
			return nil
		}
	}
	envkey, exists := l.env[lcaseKey]
	if exists {
		if val, ok := l.getEnv(envkey); ok {
			return val
		}
	}
	if nested && l.isPathShadowedInFlatMap(path, l.env) != "" {
		return nil
	}

	// Config file next
	val = l.searchMapWithPathPrefixes(l.config, path)
	if val != nil {
		return val
	}
	if nested && l.isPathShadowedInDeepMap(path, l.config) != "" {
		return nil
	}

	// K/V store next
	val = l.searchMap(l.kvstore, path)
	if val != nil {
		return val
	}
	if nested && l.isPathShadowedInDeepMap(path, l.kvstore) != "" {
		return nil
	}

	// Default next
	val = l.searchMap(l.defaults, path)
	if val != nil {
		return val
	}
	if nested && l.isPathShadowedInDeepMap(path, l.defaults) != "" {
		return nil
	}

	// last chance: if no other value is returned and a flag does exist for the value,
	// get the flag's value even if the flag's value has not changed
	if flag, exists := l.gflags[lcaseKey]; exists {
		switch flag.ValueType() {
		case "int", "int8", "int16", "int32", "int64":
			return cast.ToInt(flag.ValueString())
		case "bool":
			return cast.ToBool(flag.ValueString())
		case "stringSlice":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			res, _ := readAsCSV(s)
			return res
		case "intSlice":
			s := strings.TrimPrefix(flag.ValueString(), "[")
			s = strings.TrimSuffix(s, "]")
			res, _ := readAsCSV(s)
			return cast.ToIntSlice(res)
		default:
			return flag.ValueString()
		}
	}
	// last item, no need to check shadowing

	return nil
}

func readAsCSV(val string) ([]string, error) {
	if val == "" {
		return []string{}, nil
	}
	stringReader := strings.NewReader(val)
	csvReader := csv.NewReader(stringReader)
	return csvReader.Read()
}

// IsSet checks to see if the key has been set in any of the data locations.
// IsSet is case-insensitive for a key.
func IsSet(key string) bool { return l.IsSet(key) }
func (l *Lung) IsSet(key string) bool {
	lcaseKey := strings.ToLower(key)
	val := l.find(lcaseKey)
	return val != nil
}

// AutomaticEnv has Lung check ENV variables for all.
// keys set in config, default & flags
func AutomaticEnv() { l.AutomaticEnv() }
func (l *Lung) AutomaticEnv() {
	l.automaticEnvApplied = true
}

// SetEnvKeyReplacer sets the strings.Replacer on the lung object
// Useful for mapping an environmental variable to a key that does
// not match it.
func SetEnvKeyReplacer(r *strings.Replacer) { l.SetEnvKeyReplacer(r) }
func (l *Lung) SetEnvKeyReplacer(r *strings.Replacer) {
	l.envKeyReplacer = r
}

// RegisterAlias creates an alias that provides another accessor for the same key.
// This enables one to change a name without breaking the application.
func RegisterAlias(alias string, key string) { l.RegisterAlias(alias, key) }
func (l *Lung) RegisterAlias(alias string, key string) {
	l.registerAlias(alias, strings.ToLower(key))
}

func (l *Lung) registerAlias(alias string, key string) {
	alias = strings.ToLower(alias)
	if alias != key && alias != l.realKey(key) {
		_, exists := l.aliases[alias]

		if !exists {
			// if we alias something that exists in one of the maps to another
			// name, we'll never be able to get that value using the original
			// name, so move the config value to the new realkey.
			if val, ok := l.config[alias]; ok {
				delete(l.config, alias)
				l.config[key] = val
			}
			if val, ok := l.kvstore[alias]; ok {
				delete(l.kvstore, alias)
				l.kvstore[key] = val
			}
			if val, ok := l.defaults[alias]; ok {
				delete(l.defaults, alias)
				l.defaults[key] = val
			}
			if val, ok := l.override[alias]; ok {
				delete(l.override, alias)
				l.override[key] = val
			}
			l.aliases[alias] = key
		}
	} else {
		l.logger.Warn("Creating circular reference alias %s %s %s", viper.String("alias",alias),
			viper.String("key",key), viper.String("realKey",l.realKey(key)))
	}
}

func (l *Lung) realKey(key string) string {
	newkey, exists := l.aliases[key]
	if exists {
		l.logger.Debug("Alias %s to %s", viper.String("key",key), viper.String("newkey",newkey))
		return l.realKey(newkey)
	}
	return key
}

// InConfig checks to see if the given key (or an alias) is in the config file.
func InConfig(key string) bool { return l.InConfig(key) }
func (l *Lung) InConfig(key string) bool {
	// if the requested key is an alias, then return the proper key
	key = l.realKey(key)

	_, exists := l.config[key]
	return exists
}

// SetDefault sets the default value for this key.
// SetDefault is case-insensitive for a key.
// Default only used when no value is provided by the user via flag, config or ENV.
func SetDefault(key string, value interface{}) { l.SetDefault(key, value) }
func (l *Lung) SetDefault(key string, value interface{}) {
	// If alias passed in, then set the proper default
	key = l.realKey(strings.ToLower(key))
	value = toCaseInsensitiveValue(value)

	path := strings.Split(key, l.keyDelim)
	lastKey := strings.ToLower(path[len(path)-1])
	deepestMap := deepSearch(l.defaults, path[0:len(path)-1])

	// set innermost value
	deepestMap[lastKey] = value
}

// Set sets the value for the key in the override register.
// Set is case-insensitive for a key.
// Will be used instead of values obtained via
// flags, config file, ENV, default, or key/value store.
func Set(key string, value interface{}) { l.Set(key, value) }
func (l *Lung) Set(key string, value interface{}) {
	// If alias passed in, then set the proper override
	key = l.realKey(strings.ToLower(key))
	value = toCaseInsensitiveValue(value)

	path := strings.Split(key, l.keyDelim)
	lastKey := strings.ToLower(path[len(path)-1])
	deepestMap := deepSearch(l.override, path[0:len(path)-1])

	// set innermost value
	deepestMap[lastKey] = value
}

// ReadInConfig will discover and load the configuration file from disk
// and key/value stores, searching in one of the defined paths.
func ReadInConfig() error { return l.ReadInConfig() }
func (l *Lung) ReadInConfig() error {
	l.logger.Info("Attempting to read in config file")
	filename, err := l.getConfigFile()
	if err != nil {
		return err
	}

	if !stringInSlice(l.getConfigType(), SupportedExts) {
		return UnsupportedConfigError(l.getConfigType())
	}

	l.logger.Debug("Reading file ", viper.String("filename", filename))
	file, err := l.fs.ReadFile(filename)
	if err != nil {
		return err
	}

	config := make(map[string]interface{})

	err = l.unmarshalReader(bytes.NewReader(file), config)
	if err != nil {
		return err
	}

	l.config = config
	return nil
}

// MergeInConfig merges a new configuration with an existing config.
func MergeInConfig() error { return l.MergeInConfig() }
func (l *Lung) MergeInConfig() error {
	l.logger.Info("Attempting to merge in config file")
	filename, err := l.getConfigFile()
	if err != nil {
		return err
	}

	if !stringInSlice(l.getConfigType(), SupportedExts) {
		return UnsupportedConfigError(l.getConfigType())
	}

	file, err := l.fs.ReadFile(filename)
	if err != nil {
		return err
	}

	return l.MergeConfig(bytes.NewReader(file))
}

// ReadConfig will read a configuration file, setting existing keys to nil if the
// key does not exist in the file.
func ReadConfig(in io.Reader) error { return l.ReadConfig(in) }
func (l *Lung) ReadConfig(in io.Reader) error {
	l.config = make(map[string]interface{})
	return l.unmarshalReader(in, l.config)
}

// MergeConfig merges a new configuration with an existing config.
func MergeConfig(in io.Reader) error { return l.MergeConfig(in) }
func (l *Lung) MergeConfig(in io.Reader) error {
	cfg := make(map[string]interface{})
	if err := l.unmarshalReader(in, cfg); err != nil {
		return err
	}
	return l.MergeConfigMap(cfg)
}

// MergeConfigMap merges the configuration from the map given with an existing config.
// Note that the map given may be modified.
func MergeConfigMap(cfg map[string]interface{}) error { return l.MergeConfigMap(cfg) }
func (l *Lung) MergeConfigMap(cfg map[string]interface{}) error {
	if l.config == nil {
		l.config = make(map[string]interface{})
	}
	insensitiviseMap(cfg)
	mergeMaps(cfg, l.config, nil)
	return nil
}

// WriteConfig writes the current configuration to a file.
func WriteConfig() error { return l.WriteConfig() }
func (l *Lung) WriteConfig() error {
	filename, err := l.getConfigFile()
	if err != nil {
		return err
	}
	return l.writeConfig(filename, true)
}

// SafeWriteConfig writes current configuration to file only if the file does not exist.
func SafeWriteConfig() error { return l.SafeWriteConfig() }
func (l *Lung) SafeWriteConfig() error {
	filename, err := l.getConfigFile()
	if err != nil {
		return err
	}
	return l.writeConfig(filename, false)
}

// WriteConfigAs writes current configuration to a given filename.
func WriteConfigAs(filename string) error { return l.WriteConfigAs(filename) }
func (l *Lung) WriteConfigAs(filename string) error {
	return l.writeConfig(filename, true)
}

// SafeWriteConfigAs writes current configuration to a given filename if it does not exist.
func SafeWriteConfigAs(filename string) error { return l.SafeWriteConfigAs(filename) }
func (l *Lung) SafeWriteConfigAs(filename string) error {
	return l.writeConfig(filename, false)
}

func writeConfig(filename string, force bool) error { return l.writeConfig(filename, force) }
func (l *Lung) writeConfig(filename string, force bool) error {
	l.logger.Info("Attempting to write configuration to file.")
	ext := filepath.Ext(filename)
	if len(ext) <= 1 {
		return fmt.Errorf("Filename: %s requires valid extension.", filename)
	}
	configType := ext[1:]
	if !stringInSlice(configType, SupportedExts) {
		return UnsupportedConfigError(configType)
	}
	if l.config == nil {
		l.config = make(map[string]interface{})
	}
	flags := os.O_CREATE | os.O_TRUNC | os.O_WRONLY
	if !force {
		flags |= os.O_EXCL
	}
	f, err := l.fs.Vfs.OpenFile(filename, flags, l.configPermissions)
	if err != nil {
		return err
	}
	defer f.Close()

	if err := l.marshalWriter(f, configType); err != nil {
		return err
	}

	return f.Sync()
}

// Unmarshal a Reader into a map.
// Should probably be an unexported function.
func unmarshalReader(in io.Reader, c map[string]interface{}) error {
	return l.unmarshalReader(in, c)
}
func (l *Lung) unmarshalReader(in io.Reader, c map[string]interface{}) error {
	buf := new(bytes.Buffer)
	buf.ReadFrom(in)

	switch strings.ToLower(l.getConfigType()) {
	case "yaml", "yml":
		if err := yaml.Unmarshal(buf.Bytes(), &c); err != nil {
			return ConfigParseError{err}
		}

	case "json":
		if err := json.Unmarshal(buf.Bytes(), &c); err != nil {
			return ConfigParseError{err}
		}
	case "toml":
		tree, err := toml.LoadReader(buf)
		if err != nil {
			return ConfigParseError{err}
		}
		tmap := tree.ToMap()
		for k, v := range tmap {
			c[k] = v
		}

	case "dotenv", "env":
		env, err := env.StrictParse(buf)
		if err != nil {
			return ConfigParseError{err}
		}
		for k, v := range env {
			c[k] = v
		}

	case "properties", "props", "prop":
		l.properties = properties.NewProperties()
		var err error
		if l.properties, err = properties.Load(buf.Bytes(), properties.UTF8); err != nil {
			return ConfigParseError{err}
		}
		for _, key := range l.properties.Keys() {
			value, _ := l.properties.Get(key)
			// recursively build nested maps
			path := strings.Split(key, ".")
			lastKey := strings.ToLower(path[len(path)-1])
			deepestMap := deepSearch(c, path[0:len(path)-1])
			// set innermost value
			deepestMap[lastKey] = value
		}
	}

	insensitiviseMap(c)
	return nil
}

// Marshal a map into Writer.
func marshalWriter(f vfs.File, configType string) error {
	return l.marshalWriter(f, configType)
}
func (l *Lung) marshalWriter(f vfs.File, configType string) error {
	c := l.AllSettings()
	switch configType {
	case "json":
		b, err := json.MarshalIndent(c, "", "  ")
		if err != nil {
			return ConfigMarshalError{err}
		}
		_, err = f.WriteString(string(b))
		if err != nil {
			return ConfigMarshalError{err}
		}

	case "prop", "props", "properties":
		if l.properties == nil {
			l.properties = properties.NewProperties()
		}
		p := l.properties
		for _, key := range l.AllKeys() {
			_, _, err := p.Set(key, l.GetString(key))
			if err != nil {
				return ConfigMarshalError{err}
			}
		}
		_, err := p.WriteComment(f, "#", properties.UTF8)
		if err != nil {
			return ConfigMarshalError{err}
		}

	case "dotenv", "env":
		lines := []string{}
		for _, key := range l.AllKeys() {
			envName := strings.ToUpper(strings.Replace(key, ".", "_", -1))
			val := l.Get(key)
			lines = append(lines, fmt.Sprintf("%v=%v", envName, val))
		}
		s := strings.Join(lines, "\n")
		if _, err := f.WriteString(s); err != nil {
			return ConfigMarshalError{err}
		}

	case "toml":
		t, err := toml.TreeFromMap(c)
		if err != nil {
			return ConfigMarshalError{err}
		}
		s := t.String()
		if _, err := f.WriteString(s); err != nil {
			return ConfigMarshalError{err}
		}

	case "yaml", "yml":
		b, err := yaml.Marshal(c)
		if err != nil {
			return ConfigMarshalError{err}
		}
		if _, err = f.WriteString(string(b)); err != nil {
			return ConfigMarshalError{err}
		}
	}
	return nil
}

func keyExists(k string, m map[string]interface{}) string {
	lk := strings.ToLower(k)
	for mk := range m {
		lmk := strings.ToLower(mk)
		if lmk == lk {
			return mk
		}
	}
	return ""
}

func castToMapStringInterface(
	src map[interface{}]interface{}) map[string]interface{} {
	tgt := map[string]interface{}{}
	for k, v := range src {
		tgt[fmt.Sprintf("%v", k)] = v
	}
	return tgt
}

func castMapStringToMapInterface(src map[string]string) map[string]interface{} {
	tgt := map[string]interface{}{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

func castMapFlagToMapInterface(src map[string]FlagValue) map[string]interface{} {
	tgt := map[string]interface{}{}
	for k, v := range src {
		tgt[k] = v
	}
	return tgt
}

// mergeMaps merges two maps. The `itgt` parameter is for handling go-yaml's
// insistence on parsing nested structures as `map[interface{}]interface{}`
// instead of using a `string` as the key for nest structures beyond one level
// deep. Both map types are supported as there is a go-yaml fork that uses
// `map[string]interface{}` instead.
func mergeMaps(
	src, tgt map[string]interface{}, itgt map[interface{}]interface{}) {
	for sk, sv := range src {
		tk := keyExists(sk, tgt)
		if tk == "" {
			tgt[sk] = sv
			if itgt != nil {
				itgt[sk] = sv
			}
			continue
		}

		tv, ok := tgt[tk]
		if !ok {
			tgt[sk] = sv
			if itgt != nil {
				itgt[sk] = sv
			}
			continue
		}

		svType := reflect.TypeOf(sv)
		tvType := reflect.TypeOf(tv)
		if svType != tvType {

			continue
		}

		switch ttv := tv.(type) {
		case map[interface{}]interface{}:
			l.logger.Debug("merging maps (must convert)")
			tsv := sv.(map[interface{}]interface{})
			ssv := castToMapStringInterface(tsv)
			stv := castToMapStringInterface(ttv)
			mergeMaps(ssv, stv, ttv)
		case map[string]interface{}:
			l.logger.Debug("merging maps")
			mergeMaps(sv.(map[string]interface{}), ttv, nil)
		default:
			l.logger.Debug("setting value")
			tgt[tk] = sv
			if itgt != nil {
				itgt[tk] = sv
			}
		}
	}
}

// AllKeys returns all keys holding a value, regardless of where they are set.
// Nested keys are returned with a l.keyDelim (= ".") separator
func AllKeys() []string { return l.AllKeys() }
func (l *Lung) AllKeys() []string {
	m := map[string]bool{}
	// add all paths, by order of descending priority to ensure correct shadowing
	m = l.flattenAndMergeMap(m, castMapStringToMapInterface(l.aliases), "")
	m = l.flattenAndMergeMap(m, l.override, "")
	m = l.mergeFlatMap(m, castMapFlagToMapInterface(l.gflags))
	m = l.mergeFlatMap(m, castMapStringToMapInterface(l.env))
	m = l.flattenAndMergeMap(m, l.config, "")
	m = l.flattenAndMergeMap(m, l.kvstore, "")
	m = l.flattenAndMergeMap(m, l.defaults, "")

	// convert set of paths to list
	a := make([]string, 0, len(m))
	for x := range m {
		a = append(a, x)
	}
	return a
}

// flattenAndMergeMap recursively flattens the given map into a map[string]bool
// of key paths (used as a set, easier to manipulate than a []string):
// - each path is merged into a single key string, delimited with l.keyDelim (= ".")
// - if a path is shadowed by an earlier value in the initial shadow map,
//   it is skipped.
// The resulting set of paths is merged to the given shadow set at the same time.
func (l *Lung) flattenAndMergeMap(shadow map[string]bool, m map[string]interface{}, prefix string) map[string]bool {
	if shadow != nil && prefix != "" && shadow[prefix] {
		// prefix is shadowed => nothing more to flatten
		return shadow
	}
	if shadow == nil {
		shadow = make(map[string]bool)
	}

	var m2 map[string]interface{}
	if prefix != "" {
		prefix += l.keyDelim
	}
	for k, val := range m {
		fullKey := prefix + k
		switch val.(type) {
		case map[string]interface{}:
			m2 = val.(map[string]interface{})
		case map[interface{}]interface{}:
			m2 = cast.ToStringMap(val)
		default:
			// immediate value
			shadow[strings.ToLower(fullKey)] = true
			continue
		}
		// recursively merge to shadow map
		shadow = l.flattenAndMergeMap(shadow, m2, fullKey)
	}
	return shadow
}

// mergeFlatMap merges the given maps, excluding values of the second map
// shadowed by values from the first map.
func (l *Lung) mergeFlatMap(shadow map[string]bool, m map[string]interface{}) map[string]bool {
	// scan keys
outer:
	for k := range m {
		path := strings.Split(k, l.keyDelim)
		// scan intermediate paths
		var parentKey string
		for i := 1; i < len(path); i++ {
			parentKey = strings.Join(path[0:i], l.keyDelim)
			if shadow[parentKey] {
				// path is shadowed, continue
				continue outer
			}
		}
		// add key
		shadow[strings.ToLower(k)] = true
	}
	return shadow
}

// AllSettings merges all settings and returns them as a map[string]interface{}.
func AllSettings() map[string]interface{} { return l.AllSettings() }
func (l *Lung) AllSettings() map[string]interface{} {
	m := map[string]interface{}{}
	// start from the list of keys, and construct the map one value at a time
	for _, k := range l.AllKeys() {
		value := l.Get(k)
		if value == nil {
			// should not happen, since AllKeys() returns only keys holding a value,
			// check just in case anything changes
			continue
		}
		path := strings.Split(k, l.keyDelim)
		lastKey := strings.ToLower(path[len(path)-1])
		deepestMap := deepSearch(m, path[0:len(path)-1])
		// set innermost value
		deepestMap[lastKey] = value
	}
	return m
}

// SetFs sets the filesystem to use to read configuration.
func SetFs(fs felix.Felix) { l.SetFs(fs) }
func (l *Lung) SetFs(fs felix.Felix) {
	l.fs = fs
}

// SetConfigName sets name for the config file.
// Does not include extension.
func SetConfigName(in string) { l.SetConfigName(in) }
func (l *Lung) SetConfigName(in string) {
	if in != "" {
		l.configName = in
		l.configFile = ""
	}
}

// SetConfigType sets the type of the configuration returned by the
// remote source, e.g. "json".

func SetConfigType(in string) { l.SetConfigType(in) }
func (l *Lung) SetConfigType(in string) {
	if in != "" {
		l.configType = in
	}
}

// SetConfigPermissions sets the permissions for the config file.
func SetConfigPermissions(perm os.FileMode) { l.SetConfigPermissions(perm) }
func (l *Lung) SetConfigPermissions(perm os.FileMode) {
	l.configPermissions = perm.Perm()
}

func (l *Lung) getConfigType() string {
	if l.configType != "" {
		return l.configType
	}

	cf, err := l.getConfigFile()
	if err != nil {
		return ""
	}

	ext := filepath.Ext(cf)

	if len(ext) > 1 {
		return ext[1:]
	}

	return ""
}

func (l *Lung) getConfigFile() (string, error) {
	if l.configFile == "" {
		cf, err := l.findConfigFile()
		if err != nil {
			return "", err
		}
		l.configFile = cf
	}
	return l.configFile, nil
}

func (l *Lung) searchInPath(in string) (filename string) {
	l.logger.Debug("Searching for config in ", viper.String("path",in))
	for _, ext := range SupportedExts {
		l.logger.Debug("Checking for", viper.String("path", filepath.Join(in, l.configName+"."+ext)))
		if b, _ := exists(l.fs, filepath.Join(in, l.configName+"."+ext)); b {
			l.logger.Debug("Found: ", viper.String("path", filepath.Join(in, l.configName+"."+ext)))
			return filepath.Join(in, l.configName+"."+ext)
		}
	}

	return ""
}

// Search all configPaths for any config file.
// Returns the first path that exists (and is a config file).
func (l *Lung) findConfigFile() (string, error) {
	l.logger.Info("Searching for config in ", viper.Strings("paths",l.configPaths))

	for _, cp := range l.configPaths {
		file := l.searchInPath(cp)
		if file != "" {
			return file, nil
		}
	}
	return "", ConfigFileNotFoundError{l.configName, fmt.Sprintf("%s", l.configPaths)}
}

// Debug prints all configuration registries for debugging
// purposes.
func Debug() { l.Debug() }
func (l *Lung) Debug() {
	fmt.Printf("Aliases:\n%#v\n", l.aliases)
	fmt.Printf("Override:\n%#v\n", l.override)
	fmt.Printf("PFlags:\n%#v\n", l.gflags)
	fmt.Printf("Env:\n%#v\n", l.env)
	fmt.Printf("Key/Value Store:\n%#v\n", l.kvstore)
	fmt.Printf("Config:\n%#v\n", l.config)
	fmt.Printf("Defaults:\n%#v\n", l.defaults)
}
