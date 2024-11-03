package deej

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/jacobsa/go-serial/serial"
	"go.uber.org/zap"

	"github.com/omriharel/deej/pkg/deej/util"
)

// SerialIO provides a deej-aware abstraction layer to managing serial I/O
type SerialIO struct {
	comPort  string
	baudRate uint

	deej   *Deej
	logger *zap.SugaredLogger

	stopChannel chan bool
	connected   bool
	connOptions serial.OpenOptions
	conn        io.ReadWriteCloser

	lastKnownNumSliders        int
	currentSliderPercentValues []float32

	sliderMoveConsumers []chan SliderMoveEvent
}

// SliderMoveEvent represents a single slider move captured by deej
type SliderMoveEvent struct {
	SliderID     string
	PercentValue float32
}

var expectedLinePattern = regexp.MustCompile(`^[lrud]\n$`)

var currentSliderIndex int = 0
var currentSliderName string
var wantedValue float32 = 0.0
var isButtonHeld bool = false
var needToUpdate bool = false

// NewSerialIO creates a SerialIO instance that uses the provided deej
// instance's connection info to establish communications with the arduino chip
func NewSerialIO(deej *Deej, logger *zap.SugaredLogger) (*SerialIO, error) {
	logger = logger.Named("serial")

	sio := &SerialIO{
		deej:                deej,
		logger:              logger,
		stopChannel:         make(chan bool),
		connected:           false,
		conn:                nil,
		sliderMoveConsumers: []chan SliderMoveEvent{},
	}

	logger.Debug("Created serial i/o instance")

	// respond to config changes
	sio.setupOnConfigReload()

	return sio, nil
}

// Start attempts to connect to our arduino chip
func (sio *SerialIO) Start() error {

	// don't allow multiple concurrent connections
	if sio.connected {
		sio.logger.Warn("Already connected, can't start another without closing first")
		return errors.New("serial: connection already active")
	}

	// set minimum read size according to platform (0 for windows, 1 for linux)
	// this prevents a rare bug on windows where serial reads get congested,
	// resulting in significant lag
	minimumReadSize := 0
	if util.Linux() {
		minimumReadSize = 1
	}

	// TODO - handle all of this in the config
	// TODO - have the data/stop bits all have defaults/optional
	sio.connOptions = serial.OpenOptions{
		PortName:        sio.deej.configManager.Config.ConnectionInfo.SerialPort,
		BaudRate:        sio.deej.configManager.Config.ConnectionInfo.BaudRate,
		DataBits:        8,
		StopBits:        1,
		MinimumReadSize: uint(minimumReadSize),
	}

	sio.logger.Debugw("Attempting serial connection",
		"comPort", sio.connOptions.PortName,
		"baudRate", sio.connOptions.BaudRate,
		"minReadSize", minimumReadSize)

	var err error
	sio.conn, err = serial.Open(sio.connOptions)
	if err != nil {

		// might need a user notification here, TBD
		sio.logger.Warnw("Failed to open serial connection", "error", err)
		return fmt.Errorf("open serial connection: %w", err)
	}

	namedLogger := sio.logger.Named(strings.ToLower(sio.connOptions.PortName))

	namedLogger.Infow("Connected", "conn", sio.conn)
	sio.connected = true

	// read lines or await a stop
	go func() {
		connReader := bufio.NewReader(sio.conn)
		lineChannel := sio.readLine(namedLogger, connReader)

		for {
			select {
			case <-sio.stopChannel:
				sio.close(namedLogger)
			case line := <-lineChannel:
				sio.handleLine(namedLogger, line)
			}
		}
	}()

	return nil
}

// Stop signals us to shut down our serial connection, if one is active
func (sio *SerialIO) Stop() {
	if sio.connected {
		sio.logger.Debug("Shutting down serial connection")
		sio.stopChannel <- true
	} else {
		sio.logger.Debug("Not currently connected, nothing to stop")
	}
}

// SubscribeToSliderMoveEvents returns an unbuffered channel that receives
// a sliderMoveEvent struct every time a slider moves
func (sio *SerialIO) SubscribeToSliderMoveEvents() chan SliderMoveEvent {
	ch := make(chan SliderMoveEvent)
	sio.sliderMoveConsumers = append(sio.sliderMoveConsumers, ch)

	return ch
}

func (sio *SerialIO) setupOnConfigReload() {
	configReloadedChannel := sio.deej.configManager.SubscribeToChanges()

	const stopDelay = 50 * time.Millisecond

	go func() {
		for {
			select {
			case <-configReloadedChannel:

				// make any config reload unset our slider number to ensure process volumes are being re-set
				// (the next read line will emit SliderMoveEvent instances for all sliders)\
				// this needs to happen after a small delay, because the session map will also re-acquire sessions
				// whenever the config file is reloaded, and we don't want it to receive these move events while the map
				// is still cleared. this is kind of ugly, but shouldn't cause any issues
				go func() {
					<-time.After(stopDelay)
					sio.lastKnownNumSliders = 0
				}()

				// if connection params have changed, attempt to stop and start the connection
				if sio.deej.configManager.Config.ConnectionInfo.SerialPort != sio.connOptions.PortName ||
					uint(sio.deej.configManager.Config.ConnectionInfo.BaudRate) != sio.connOptions.BaudRate {

					sio.logger.Info("Detected change in connection parameters, attempting to renew connection")
					sio.Stop()

					// let the connection close
					<-time.After(stopDelay)

					if err := sio.Start(); err != nil {
						sio.logger.Warnw("Failed to renew connection after parameter change", "error", err)
					} else {
						sio.logger.Debug("Renewed connection successfully")
					}
				}
			}
		}
	}()
}

func (sio *SerialIO) close(logger *zap.SugaredLogger) {
	if err := sio.conn.Close(); err != nil {
		logger.Warnw("Failed to close serial connection", "error", err)
	} else {
		logger.Debug("Serial connection closed")
	}

	sio.conn = nil
	sio.connected = false
}

func (sio *SerialIO) readLine(logger *zap.SugaredLogger, reader *bufio.Reader) chan string {
	ch := make(chan string)

	go func() {
		for {
			line, err := reader.ReadString('\n')
			if err != nil {

				if sio.deej.Verbose() {
					logger.Warnw("Failed to read line from serial", "error", err, "line", line)
				}

				// just ignore the line, the read loop will stop after this
				return
			}

			if sio.deej.Verbose() {
				logger.Debugw("Read new line", "line", line)
			}

			// deliver the line to the channel
			ch <- line
		}
	}()

	return ch
}

func (sio *SerialIO) handleLine(logger *zap.SugaredLogger, line string) {

	// this function receives an unsanitized line which is guaranteed to end with LF,
	// but most lines will end with CRLF. it may also have garbage instead of
	// deej-formatted values, so we must check for that! just ignore bad ones
	if !expectedLinePattern.MatchString(line) {
		return
	}

	// trim the suffix
	line = strings.TrimSuffix(line, "\n")
	// logger.Debugf("Got input '%s'", line)

	// Initial fetch to avoid 0 value by default.
	// if needToFetchCurrentLevel {
	// 	currentValue = sio.currentSliderPercentValues[currentSlider]
	// 	needToFetchCurrentLevel = false
	// }
	switch line {
	case "l":
		if isButtonHeld {
			logger.Debug("Channel previous")
			currentSliderIndex--
			if currentSliderIndex < 0 {
				currentSliderIndex = 0
			}
			sliderMapping, _ := sio.deej.configManager.getSliderMappingByIndex(currentSliderIndex)
			wantedValue = sliderMapping.Volume

			currentSliderName, _ = sio.deej.configManager.getSliderMappingKeyByIndex(currentSliderIndex)
			logger.Debugf("Channel: %d %s", currentSliderIndex, currentSliderName)
		} else {
			sliderMapping, _ := sio.deej.configManager.getSliderMappingByKey(currentSliderName)
			wantedValue = sliderMapping.Volume - 0.01
			if wantedValue < 0.0 {
				wantedValue = 0.0
			}
			needToUpdate = true
			logger.Debugf("Lowering slider %d %s volume %d", currentSliderIndex, currentSliderName, wantedValue)
		}
	case "r":
		if isButtonHeld {
			logger.Debug("Channel next")
			currentSliderIndex++
			// why was 1024 specifically hardcoded originally in deej?
			if currentSliderIndex > 1024 {
				currentSliderIndex = 1024
			}
			sliderMappingCount := sio.deej.configManager.getSliderMappingCount()
			if currentSliderIndex > sliderMappingCount {
				currentSliderIndex = sliderMappingCount
			}

			sliderMapping, _ := sio.deej.configManager.getSliderMappingByIndex(currentSliderIndex)
			wantedValue = sliderMapping.Volume

			currentSliderName, _ = sio.deej.configManager.getSliderMappingKeyByIndex(currentSliderIndex)
			logger.Debugf("Channel: %d %s", currentSliderIndex, currentSliderName)
		} else {
			sliderMapping, _ := sio.deej.configManager.getSliderMappingByKey(currentSliderName)
			wantedValue = sliderMapping.Volume + 0.01
			if wantedValue > 1.0 {
				wantedValue = 1.0
			}

			needToUpdate = true
			logger.Debugf("Raising slider %d %s volume %d", currentSliderIndex, currentSliderName, wantedValue)
		}
	case "d":
		logger.Debug("Selecting channel")
		isButtonHeld = true
		// logger.Debugf("Num sliders %d", len(sio.deej.config.SliderMapping))
		keys, _ := sio.deej.configManager.getSliderMappingKeys()
		logger.Debugf("Sliders %+s", keys)

		needToUpdate = false
	case "u":
		logger.Debug("Selecting volume")
		isButtonHeld = false
		// TODO - get current value and assign to both so it doesn't reset
		// TODO - get average of values?
		needToUpdate = false
		currentSliderName, _ = sio.deej.configManager.getSliderMappingKeyByIndex(currentSliderIndex)
		// currentValue = sio.deej.serial.currentSliderPercentValues[currentSlider]

	default:
		logger.Warnf("Unhandled input \"%s\"", line)
	}

	// for each slider:
	moveEvents := []SliderMoveEvent{}

	sliderMapping, _ := sio.deej.configManager.getSliderMappingByIndex(currentSliderIndex)
	if needToUpdate && (wantedValue != sliderMapping.Volume) {
		moveEvents = append(moveEvents, SliderMoveEvent{
			SliderID:     currentSliderName,
			PercentValue: wantedValue,
		})
		// sio.deej.config.Config.SliderMappings[currentSlider].Volume = wantedValue
	}

	if sio.deej.Verbose() {
		for _, event := range moveEvents {
			logger.Debugw("Slider moved", "event", event)
		}
	}

	// deliver move events if there are any, towards all potential consumers
	if len(moveEvents) > 0 {
		for _, consumer := range sio.sliderMoveConsumers {
			for _, moveEvent := range moveEvents {
				consumer <- moveEvent
				// currentSliderValues[moveEvent.SliderID] = moveEvent.PercentValue
				// TODO use a local function in config manager to lock/update the values
				sm, _ := sio.deej.configManager.getSliderMappingByKey(moveEvent.SliderID)
				sm.Volume = moveEvent.PercentValue
				sio.deej.configManager.UpdateSliderMappingByKey(moveEvent.SliderID, sm)
			}
		}
	}
}
