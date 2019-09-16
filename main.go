package main

import (
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"regexp"
	"strconv"
	"time"

	"github.com/joeshaw/envdecode"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
)

type Config struct {
	EnvoyHost           string `env:"ENVOY_HOST,default=http://127.0.0.1"`
	EnvoyPort           int    `env:"ENVOY_PORT,default=15000"`
	EnvoyWaitUntilLive  bool   `env:"ENVOY_WAIT_UNTIL_LIVE,default=true"`
	EnvoyWaitForCDSPush bool   `env:"ENVOY_WAIT_FOR_CDS_PUSH,default=true"`
	EnvoyWaitForLDSPush bool   `env:"ENVOY_WAIT_FOR_LDS_PUSH,default=true"`
	EntryPoint          string
}

func getConfig() (*Config, error) {
	config := Config{}
	err := envdecode.Decode(&config)
	if err != nil {
		return nil, err
	}

	if len(os.Args) < 2 {
		return nil, errors.New("Must supply an entry point as the first argument")
	}

	entrypoint, err := exec.LookPath(os.Args[1])
	if err != nil {
		return nil, errors.Wrap(err, fmt.Sprintf("entry point '%v' could not be found", entrypoint))
	}
	config.EntryPoint = entrypoint

	return &config, nil
}

func main() {
	logger := logrus.New()
	logger.Formatter = &logrus.TextFormatter{
		TimestampFormat: "2006-01-02T15:04:05.000Z07:00",
	}
	logger.Level = logrus.InfoLevel
	logger.WithField("component", "envoy-lifecycle")

	config, err := getConfig()
	if err != nil {
		logger.WithError(err).Fatal("failed to get configuration")
	}
	logger.Infof("Configuration: %+v", config)

	if config.EnvoyWaitUntilLive {
		logger.Info("Checking Envoy is LIVE")
		config.CheckLive(logger)
	}

	config.CheckXDSSuccess(logger)

	logger.Info("Checks successful. Handing over to entrypoint")
	exitCode, err := config.Run(logger)
	if err != nil {
		logger.WithError(err).Fatal("Failed to execute entrypoint")
	}

	logger.Infof("Entrypoint exited with status code: %v", *exitCode)
	os.Exit(*exitCode)
}

type ServerInfo struct {
	State string `json:"state"`
}

func (config *Config) CheckLive(logger *logrus.Logger) {
	var serverInfo = ServerInfo{}
	var err error
	var resp *http.Response
	infoUrl := fmt.Sprintf("%s:%v/server_info", config.EnvoyHost, config.EnvoyPort)

	Retry(logger, func() error {
		resp, err = http.Get(infoUrl)
		if err != nil {
			return errors.Wrap(err, "Envoy cannot be reached")
		}

		defer resp.Body.Close()
		err = json.NewDecoder(resp.Body).Decode(&serverInfo)
		if err != nil {
			return errors.Wrap(err, "Failed to unmarshal response body")
		}

		if serverInfo.State != "LIVE" {
			return errors.New("Envoy not LIVE yet")
		}

		return nil
	})
}

func Retry(logger *logrus.Logger, f func() error) {
	var err error
	retryPeriod := 1 * time.Second
	for {
		err = f()
		if err == nil {
			return
		}
		logger.WithError(err).Errorf("Retry failed. Waiting %v", retryPeriod)
		time.Sleep(retryPeriod)
	}
}

type Stats struct {
	CDSUpdatedSuccessfully bool
	LDSUpdatedSuccessfully bool
}

var cdsRegex = regexp.MustCompile(`cluster_manager\.cds\.update_success: (\d)`)
var ldsRegex = regexp.MustCompile(`listener_manager\.lds\.update_success: (\d)`)

func ParseStats(bs []byte) (*Stats, error) {
	cdsMatch := cdsRegex.FindSubmatch(bs)
	if len(cdsMatch) != 2 {
		return nil, errors.New("could not match cds update success")
	}
	cds, err := strconv.Atoi(string(cdsMatch[1]))
	if err != nil {
		return nil, errors.New("could not parse cds update success as int")
	}

	ldsMatch := ldsRegex.FindSubmatch(bs)
	if len(ldsMatch) != 2 {
		return nil, errors.New("could not match lds update success")
	}
	lds, err := strconv.Atoi(string(ldsMatch[1]))
	if err != nil {
		return nil, errors.New("could not parse lds update success as int")
	}

	return &Stats{
		CDSUpdatedSuccessfully: 0 < cds,
		LDSUpdatedSuccessfully: 0 < lds,
	}, nil
}

func (config *Config) CheckXDSSuccess(logger *logrus.Logger) {
	var err error
	var stats = &Stats{}
	var body []byte
	var resp *http.Response
	statsUrl := fmt.Sprintf("%s:%v/stats", config.EnvoyHost, config.EnvoyPort)

	if !config.EnvoyWaitForCDSPush && !config.EnvoyWaitForLDSPush {
		return
	}

	Retry(logger, func() error {
		resp, err = http.Get(statsUrl)
		if err != nil {
			return errors.Wrap(err, "Envoy cannot be reached")
		}

		defer resp.Body.Close()
		body, err = ioutil.ReadAll(resp.Body)
		if err != nil {
			return errors.Wrap(err, "Failed to read response body")
		}

		stats, err = ParseStats(body)
		if err != nil {
			return errors.Wrap(err, "Failed to parse response body")
		}

		if config.EnvoyWaitForLDSPush && !stats.LDSUpdatedSuccessfully {
			return errors.New(fmt.Sprintf("Envoy has not successfully received a LDS push yet: %+v", stats))
		}

		if config.EnvoyWaitForCDSPush && !stats.CDSUpdatedSuccessfully {
			return errors.New(fmt.Sprintf("Envoy has not successfully received a CDS push yet: %+v", stats))
		}

		return nil
	})
}

// ForwardSignals listens for all OS signals and forwards them to the process
func ForwardSignals(logger *logrus.Logger, process *os.Process) {
	stop := make(chan os.Signal, 2)
	signal.Notify(stop)
	for sig := range stop {
		if process == nil {
			logger.Fatal("%v signal received but the entrypoint has not started")
		}

		process.Signal(sig)
	}
}

func (config *Config) StartEntrypoint() (*os.Process, error) {
	process, err := os.StartProcess(config.EntryPoint, os.Args[1:], &os.ProcAttr{
		Files: []*os.File{os.Stdin, os.Stdout, os.Stderr},
	})

	if err != nil {
		return nil, err
	}

	return process, nil
}

// Run the entrypoint, passing down any signals
func (config *Config) Run(logger *logrus.Logger) (*int, error) {
	var process *os.Process

	go ForwardSignals(logger, process)

	process, err := config.StartEntrypoint()
	if err != nil {
		return nil, errors.New("Failed to start entrypoint")
	}

	processState, err := process.Wait()
	if err != nil {
		return nil, errors.Wrap(err, "Failed waiting for process")
	}

	exitCode := processState.ExitCode()
	return &exitCode, nil
}
