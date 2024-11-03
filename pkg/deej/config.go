package deej

import (
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/fsnotify/fsnotify"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// ConnectionInfo represents the settings for connecting to the Arduino board
type ConnectionInfo struct {
	SerialPort string `yaml:"serial_port"`
	BaudRate   uint   `yaml:"baud_rate"`
}

// SliderMapping represents the mapping of sliders
type SliderMapping struct {
	Volume  float32  `yaml:"volume"`
	Muted   bool     `yaml:"muted"`
	Targets []string `yaml:"targets"`
}

// Config represents the entire configuration structure
type Config struct {
	SliderMappings      map[string]SliderMapping `yaml:"slider_mappings"`
	InvertSliders       bool                     `yaml:"invert_sliders"`
	ConnectionInfo      ConnectionInfo           `yaml:"connection_info"`
	NoiseReductionLevel string                   `yaml:"noise_reduction_level"`
	ConfigSaveInterval  int                      `yaml:"config_save_interval"`
}

// ConfigManager manages config loading, watching, and notifying subscribers on changes
type ConfigManager struct {
	Config             *Config
	orderedSliderKeys  []string
	logger             *zap.SugaredLogger
	notifier           Notifier
	stopWatcherChannel chan bool
	reloadConsumers    []chan bool
	configFilePath     string
	lock               sync.Locker
	configModified     bool
}

// NewConfigManager creates a new ConfigManager instance
func NewConfigManager(logger *zap.SugaredLogger, notifier Notifier, configFilePath string) (*ConfigManager, error) {
	logger = logger.Named("config")

	cm := &ConfigManager{
		logger:             logger,
		notifier:           notifier,
		stopWatcherChannel: make(chan bool),
		reloadConsumers:    []chan bool{},
		configFilePath:     configFilePath,
		lock:               &sync.Mutex{},
	}

	logger.Debug("Created config manager instance")
	return cm, nil
}

// Load loads the configuration file into the Config struct
// Update the Load function to store keys in the order they appear in YAML
func (cm *ConfigManager) Load() error {
	cm.logger.Debugw("Loading config", "path", cm.configFilePath)

	file, err := os.Open(cm.configFilePath)
	if err != nil {
		if os.IsNotExist(err) {
			cm.logger.Warnw("Config file not found", "path", cm.configFilePath)
			cm.notifier.Notify("Can't find configuration!", fmt.Sprintf("%s must be in the directory. Please re-launch", cm.configFilePath))
			return fmt.Errorf("config file doesn't exist: %s", cm.configFilePath)
		}
		return fmt.Errorf("error opening config file: %w", err)
	}
	defer file.Close()

	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)

	// What's the point of having defaults? It could be different on any system.
	cm.Config = &Config{
		ConfigSaveInterval: 60,
		// Set default values
		ConnectionInfo: ConnectionInfo{
			SerialPort: "COM4",
			BaudRate:   9600,
		},
	}

	if err := decoder.Decode(cm.Config); err != nil {
		cm.logger.Warnw("Failed to decode config", "error", err)
		return fmt.Errorf("failed to decode config: %w", err)
	}

	// Populate orderedSliderKeys based on SliderMappings
	cm.orderedSliderKeys = make([]string, 0, len(cm.Config.SliderMappings))
	for key := range cm.Config.SliderMappings {
		cm.orderedSliderKeys = append(cm.orderedSliderKeys, key)
	}

	cm.logger.Infof("Config loaded successfully with ordered keys: %+v", cm.orderedSliderKeys)
	return nil
}

func (cm *ConfigManager) SaveConfig() error {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	// Open the file for writing
	file, err := os.Create(cm.configFilePath)
	if err != nil {
		cm.logger.Warnw("Failed to open config file for writing", "error", err)
		return fmt.Errorf("failed to open config file for writing: %w", err)
	}
	defer file.Close()

	encoder := yaml.NewEncoder(file)
	defer encoder.Close()

	// Write the current configuration to the file
	if err := encoder.Encode(cm.Config); err != nil {
		cm.logger.Warnw("Failed to encode config to file", "error", err)
		return fmt.Errorf("failed to encode config to file: %w", err)
	}

	// Reset the modified flag
	cm.configModified = false
	cm.logger.Info("Config saved successfully to disk")

	return nil
}

func (cm *ConfigManager) PeriodicallySaveConfig(interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			var shouldSave bool

			// Check if config is modified inside the lock
			// TODO - separate function to get "shouldSave"?
			// Possibly just call save instead and have that function handle it since it already has it's own lock.
			// Ran into a deadlock here by locking then calling save while locked, so above would be good change.
			cm.lock.Lock()
			if cm.configModified {
				cm.logger.Info("Config modified, preparing to save...")
				shouldSave = true
			}
			cm.lock.Unlock() // Release lock before saving to avoid deadlock

			// If modified, save the config
			if shouldSave {
				if err := cm.SaveConfig(); err != nil {
					cm.logger.Warnw("Failed to save config to disk", "error", err)
				} else {
					cm.logger.Info("Config saved to disk")
				}
			}

		case <-cm.stopWatcherChannel:
			cm.logger.Debug("Stopping periodic config save")
			return
		}
	}
}

// SubscribeToChanges allows external components to subscribe to config reload notifications
func (cm *ConfigManager) SubscribeToChanges() chan bool {
	c := make(chan bool)
	cm.reloadConsumers = append(cm.reloadConsumers, c)
	return c
}

// WatchConfigFileChanges starts watching the configuration file for changes and reloads it when modified
func (cm *ConfigManager) WatchConfigFileChanges() {
	cm.logger.Debugw("Watching config file for changes", "path", cm.configFilePath)

	const minTimeBetweenReloads = 500 * time.Millisecond
	const delayAfterChange = 50 * time.Millisecond

	lastReload := time.Now()

	watcher, err := fsnotify.NewWatcher()
	if err != nil {
		cm.logger.Errorw("Failed to create file watcher", "error", err)
		return
	}
	defer watcher.Close()

	if err := watcher.Add(cm.configFilePath); err != nil {
		cm.logger.Errorw("Failed to watch config file", "path", cm.configFilePath, "error", err)
		return
	}

	for {
		select {
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			if event.Op&fsnotify.Write == fsnotify.Write {
				now := time.Now()

				if lastReload.Add(minTimeBetweenReloads).Before(now) {
					cm.logger.Debugw("Config file modified, reloading", "event", event)

					time.Sleep(delayAfterChange)

					if err := cm.Load(); err != nil {
						cm.logger.Warnw("Failed to reload config", "error", err)
					} else {
						cm.logger.Info("Config reloaded successfully")
						cm.notifier.Notify("Configuration reloaded!", "Your changes have been applied.")
						cm.notifySubscribers()
					}
					lastReload = now
				}
			}
		case <-cm.stopWatcherChannel:
			cm.logger.Debug("Stopping config file watcher")
			return
		}
	}
}

// Function to get the key by index using the ordered keys slice
func (cm *ConfigManager) getSliderMappingByKey(key string) (SliderMapping, error) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	mapping, exists := cm.Config.SliderMappings[key]
	if !exists {
		return SliderMapping{}, fmt.Errorf("slider mapping with key '%s' not found", key)
	}
	return mapping, nil
}

// Function to get the key by index using the ordered keys slice
func (cm *ConfigManager) getSliderMappingByIndex(index int) (SliderMapping, error) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if index < 0 || index >= len(cm.orderedSliderKeys) {
		return SliderMapping{}, fmt.Errorf("invalid index '%d'", index)
	}
	key := cm.orderedSliderKeys[index]
	return cm.Config.SliderMappings[key], nil
}

// Function to get the key by index using the ordered keys slice, with error handling
func (cm *ConfigManager) getSliderMappingKeys() ([]string, error) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if len(cm.orderedSliderKeys) == 0 {
		return nil, fmt.Errorf("no slider mapping keys available")
	}

	// Return a copy of the orderedSliderKeys to avoid external modification
	keys := make([]string, len(cm.orderedSliderKeys))
	copy(keys, cm.orderedSliderKeys)

	return keys, nil
}

func (cm *ConfigManager) getSliderMappingCount() int {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	return len(cm.Config.SliderMappings)
}

// Function to get the key by index using the ordered keys slice, with error handling
func (cm *ConfigManager) getSliderMappingKeyByIndex(index int) (string, error) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if index < 0 || index >= len(cm.orderedSliderKeys) {
		return "", fmt.Errorf("index %d is out of range", index)
	}

	return cm.orderedSliderKeys[index], nil
}

func (cm *ConfigManager) UpdateSliderMappingByKey(key string, mapping SliderMapping) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	cm.Config.SliderMappings[key] = mapping
	cm.configModified = true
	cm.logger.Debugw("Updated slider mapping", "key", key)
}

func (cm *ConfigManager) UpdateSliderMappingByIndex(index int, mapping SliderMapping) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	var key string = cm.orderedSliderKeys[index]
	cm.Config.SliderMappings[key] = mapping
	cm.configModified = true
	cm.logger.Debugw("Updated slider mapping", "key", key)
}

// StopWatchingConfigFile stops watching the configuration file
func (cm *ConfigManager) StopWatchingConfigFile() {
	cm.stopWatcherChannel <- true
}

// notifySubscribers notifies all subscribed components of a config reload
func (cm *ConfigManager) notifySubscribers() {
	cm.logger.Debug("Notifying subscribers about config reload")
	for _, subscriber := range cm.reloadConsumers {
		subscriber <- true
	}
}

// Function to retrieve all slider mappings in the order of orderedSliderKeys
func (cm *ConfigManager) getSliderMappings() ([]SliderMapping, error) {
	cm.lock.Lock()
	defer cm.lock.Unlock()

	if len(cm.orderedSliderKeys) == 0 {
		return nil, fmt.Errorf("no slider mappings available")
	}

	mappings := make([]SliderMapping, 0, len(cm.orderedSliderKeys))
	for _, key := range cm.orderedSliderKeys {
		if mapping, exists := cm.Config.SliderMappings[key]; exists {
			mappings = append(mappings, mapping)
		} else {
			return nil, fmt.Errorf("slider mapping for key '%s' not found", key)
		}
	}

	return mappings, nil
}
func (cm *ConfigManager) StopPeriodicSave() {
	cm.stopWatcherChannel <- true
}
