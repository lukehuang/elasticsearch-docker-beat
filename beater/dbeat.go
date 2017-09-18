package beater

import (
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/Axway/elasticsearch-docker-beat/config"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/client"
	"github.com/elastic/beats/libbeat/beat"
	"github.com/elastic/beats/libbeat/common"
	"github.com/elastic/beats/libbeat/logp"
	"github.com/elastic/beats/libbeat/publisher"
)

// dbeat the amp beat struct
type dbeat struct {
	done                chan struct{}
	config              config.Config
	client              publisher.Client
	dockerClient        *client.Client
	eventStreamReading  bool
	containers          map[string]*ContainerData
	lastUpdate          time.Time
	logsSavedDatePeriod int
	nbLogs              int
	nbMetrics           int
	MLDefault           *config.MLConfig
	MLStackMap          map[string]*config.MLConfig
	MLServiceMap        map[string]*config.MLConfig
	MLContainerMap      map[string]*config.MLConfig
	beaterStarted       bool
}

// New Creates beater
func New(b *beat.Beat, cfg *common.Config) (beat.Beater, error) {
	fmt.Println("dbeat version 0.0.2 b10")
	bt := &dbeat{
		done:           make(chan struct{}),
		MLStackMap:     make(map[string]*config.MLConfig),
		MLServiceMap:   make(map[string]*config.MLConfig),
		MLContainerMap: make(map[string]*config.MLConfig),
	}
	dconfig := config.DefaultConfig
	if err := cfg.Unpack(&dconfig); err != nil {
		return nil, fmt.Errorf("Error reading config file: %v", err)
	}
	bt.config = dconfig
	return bt, nil
}

// set log multiline configuration
func (bt *dbeat) setMLConfig() {
	for mlName, mlMap := range bt.config.LogsMultiline {
		ml := &config.MLConfig{Activated: true, Negate: false, Append: true}
		applyOn := ""
		if mlMap != nil {
			for name, value := range mlMap {
				if strings.ToLower(name) == "activated" {
					if strings.ToLower(value) == "false" {
						ml.Activated = false
					}
				}
				if strings.ToLower(name) == "applyon" {
					applyOn = strings.ToLower(value)
				}
				if strings.ToLower(name) == "pattern" {
					ml.Pattern = value
				}
				if strings.ToLower(name) == "negate" {
					if strings.ToLower(value) == "true" {
						ml.Negate = true
					}
				}
				if strings.ToLower(name) == "append" {
					if strings.ToLower(value) == "false" {
						ml.Append = false
					}
				}
				if strings.ToLower(mlName) == "default" {
					bt.MLDefault = ml
				} else if applyOn == "container" {
					bt.MLContainerMap["/"+mlName] = ml
				} else if applyOn == "service" {
					bt.MLServiceMap["/"+mlName] = ml
				} else if applyOn == "stack" {
					bt.MLStackMap["/"+mlName] = ml
				}
			}
		}
		fmt.Printf("ML apply on %s name=%s: %+v\n", applyOn, mlName, ml)
	}
	fmt.Printf("MLContainer setting: %+v\n", bt.MLContainerMap)
}

// set custom label configuration using env variable
func (bt *dbeat) setCLConfig() {
	if cs := os.Getenv("CUSTOM_LABELS"); cs != "" {
		fmt.Printf("Custom labels: %s\n", cs)
		bt.config.CustomLabels = make([]string, 0)
		list := strings.Split(cs, ",")
		for _, pattern := range list {
			bt.config.CustomLabels = append(bt.config.CustomLabels, strings.TrimSpace(pattern))
		}
	}
}

// set custom label configuration using env variable
func (bt *dbeat) setExcludedConfig() {
	if cs := os.Getenv("EXCLUDED_CONTAINERS"); cs != "" {
		fmt.Printf("Excluded containers: %s\n", cs)
		bt.config.ExcludedContainers = make([]string, 0)
		list := strings.Split(cs, ",")
		for _, pattern := range list {
			bt.config.ExcludedContainers = append(bt.config.ExcludedContainers, strings.TrimSpace(pattern))
		}
	}
	if cs := os.Getenv("EXCLUDED_SERVICES"); cs != "" {
		fmt.Printf("Excluded service: %s\n", cs)
		bt.config.ExcludedServices = make([]string, 0)
		list := strings.Split(cs, ",")
		for _, pattern := range list {
			bt.config.ExcludedServices = append(bt.config.ExcludedServices, strings.TrimSpace(pattern))
		}
	}
	if cs := os.Getenv("EXCLUDED_STACKS"); cs != "" {
		fmt.Printf("Excluded stacks: %s\n", cs)
		bt.config.ExcludedStacks = make([]string, 0)
		list := strings.Split(cs, ",")
		for _, pattern := range list {
			bt.config.ExcludedStacks = append(bt.config.ExcludedStacks, strings.TrimSpace(pattern))
		}
	}
}

// Run dbeat main loop
func (bt *dbeat) Run(b *beat.Beat) error {
	logp.Info("starting dbeat")
	bt.client = b.Publisher.Connect()
	ticker := time.NewTicker(bt.config.Period)
	err := bt.start(&bt.config)
	if err != nil {
		log.Fatal(err)
	}
	bt.setMLConfig()
	bt.setCLConfig()
	bt.setExcludedConfig()
	bt.initAPI()
	fmt.Printf("Config: %+v\n", bt.config)
	bt.beaterStarted = true
	bt.containers = make(map[string]*ContainerData)
	ContainerListOptions := types.ContainerListOptions{All: true}
	containers, err := bt.dockerClient.ContainerList(context.Background(), ContainerListOptions)
	if err != nil {
		return err
	}
	for _, cont := range containers {
		bt.addContainer(cont.ID)
	}
	bt.lastUpdate = time.Now()
	fmt.Println("dbeat is running! Hit CTRL-C to stop it.")

	for {
		select {
		case <-bt.done:
			return nil
		case <-ticker.C:
		}

		bt.tick()
	}
}

// Stop dbeat stop
func (bt *dbeat) Stop() {
	bt.client.Close()
	bt.Close()
	close(bt.done)
}
