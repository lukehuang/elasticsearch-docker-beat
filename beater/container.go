package beater

import (
	"context"
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"regexp"
	"strings"
	"time"

	"github.com/Axway/elasticsearch-docker-beat/config"
	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/events"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/client"
)

const (
	defaultTimeOut   = 30 * time.Second
	dockerAPIVersion = "1.24"
)

//ContainerData data
type ContainerData struct {
	//container metadata
	name            string
	ID              string
	shortName       string
	serviceName     string
	serviceID       string
	stackName       string
	nodeID          string
	pid             int
	state           string
	health          string
	axwayTargetFlow string
	hostIP          string
	hostname        string
	//runtime variable
	tobepurged       bool
	logsStream       io.ReadCloser
	logsReadError    bool
	metricsStream    io.ReadCloser
	metricsReadError bool
	previousIOStats  *IOStats
	previousNetStats *NetStats
	lastDateSaveTime time.Time
	lastLog          string
	sdate            string
	lastLogTimestamp time.Time
	lastLogTime      time.Time
	lastLogAbsolute  time.Time
	//container config
	mlConfig        *config.MLConfig
	customLabelsMap map[string]string
	plainFilters    []string
}

//AgentStart Connect to docker engine, get initial containers list and start the agent
func (a *dbeat) start(config *config.Config) error {
	// Connection to Docker
	os.MkdirAll(containersDateDir, 0666)
	defaultHeaders := map[string]string{"User-Agent": "dbeat"}
	cli, err := client.NewClient(config.DockerURL, dockerAPIVersion, nil, defaultHeaders)
	if err != nil {
		return err
	}
	a.dockerClient = cli
	log.Println("Connected to Docker-engine")
	time.Sleep(10 * time.Second)
	log.Println("Extracting containers list...")
	log.Println("done")
	return nil
}

//starts logs and metrics stream of eech new started container
func (a *dbeat) tick() {
	if !a.beaterStarted {
		return
	}
	if a.config.Logs {
		log.Printf("logs sent during last period: %d\n", a.nbLogs)
		a.nbLogs = 0
		a.updateLogsStream()
	}
	if a.config.Memory || a.config.Net || a.config.IO || a.config.CPU {
		log.Printf("metrics sent during last period: %d\n", a.nbMetrics)
		a.nbMetrics = 0
		a.updateMetricsStream()
	}
	a.updateEventsStream()
}

//Verify if the event stream is working, if not start it
func (a *dbeat) updateEventsStream() {
	if !a.eventStreamReading {
		log.Println("Opening docker events stream...")
		args := filters.NewArgs()
		args.Add("type", "container")
		args.Add("event", "die")
		args.Add("event", "stop")
		args.Add("event", "destroy")
		args.Add("event", "kill")
		args.Add("event", "create")
		args.Add("event", "start")
		eventsOptions := types.EventsOptions{Filters: args}
		stream, err := a.dockerClient.Events(context.Background(), eventsOptions)
		a.startEventStream(stream, err)
	}
}

// Start and read the docker event stream and update container list accordingly
func (a *dbeat) startEventStream(stream <-chan events.Message, errs <-chan error) {
	a.eventStreamReading = true
	log.Println("start events stream reader")
	go func() {
		for {
			select {
			case err := <-errs:
				if err != nil {
					log.Printf("Error reading event: %v\n", err)
					a.eventStreamReading = false
					return
				}
			case event := <-stream:
				log.Printf("Docker event: action=%s containerId=%s\n", event.Action, event.Actor.ID)
				a.updateContainerMap(event.Action, event.Actor.ID)
			}
		}
	}()
}

//Update containers list concidering event action and event containerId
func (a *dbeat) updateContainerMap(action string, containerID string) {
	if action == "start" {
		a.addContainer(containerID)
	} else if action == "destroy" || action == "die" || action == "kill" || action == "stop" {
		go func() {
			time.Sleep(5 * time.Second)
			a.removeContainer(containerID)
		}()
	}
}

//add a container to the main container map and retrieve some container information
func (a *dbeat) addContainer(ID string) {
	_, ok := a.containers[ID]
	if !ok {
		inspect, err := a.dockerClient.ContainerInspect(context.Background(), ID)
		if err == nil {
			data := ContainerData{
				ID:              ID,
				name:            inspect.Name,
				state:           inspect.State.Status,
				pid:             inspect.State.Pid,
				health:          "",
				logsStream:      nil,
				logsReadError:   false,
				tobepurged:      false,
				lastLog:         "",
				lastLogTime:     time.Now(),
				customLabelsMap: make(map[string]string),
				plainFilters:    a.config.LogsPlainFilters,
			}
			log.Printf("Container %s state: %s\n", data.name, data.state)
			if data.state == "exited" || data.state == "dead" {
				return
			}
			data.name = strings.Replace(data.name, "/", "", 1)
			data.name = strings.TrimSpace(data.name)
			labels := inspect.Config.Labels
			if a.config.MappingOnContainerName {
				list := strings.Split(data.name, "_")
				if len(list) >= 2 {
					data.stackName = list[0]
					data.serviceName = list[1]
				} else {
					data.serviceName = "noService"
					data.stackName = "noStack"
				}
			} else {
				data.serviceName = strings.TrimPrefix(labels["com.docker.swarm.service.name"], labels["com.docker.stack.namespace"]+"_")
				if data.serviceName == "" {
					data.serviceName = "noService"
				}
				data.shortName = fmt.Sprintf("%s_%d", data.serviceName, data.pid)
				data.serviceID = a.getMapValue(labels, "com.docker.swarm.service.id")
				data.nodeID = a.getMapValue(labels, "com.docker.swarm.node.id")
				data.stackName = a.getMapValue(labels, "com.docker.stack.namespace")
				if data.stackName == "" {
					data.stackName = "noStack"
				}
			}
			data.hostIP = a.hostIP
			data.hostname = a.hostname
			if inspect.State.Health != nil {
				data.health = inspect.State.Health.Status
			}
			if a.isExcluded(&data) {
				return
			}
			a.setMultilineSetting(&data)
			log.Printf("Multiline setting: %+v\n", data.mlConfig)
			a.setPlainFiltersSetting(&data)
			log.Printf("Plain filter setting: %+v\n", data.plainFilters)
			for _, pattern := range a.config.CustomLabels {
				for labelName, labelValue := range labels {
					if ok, _ := regexp.MatchString(pattern, labelName); ok {
						data.customLabelsMap[labelName] = labelValue
					}
				}
			}
			a.containers[ID] = &data
		} else {
			log.Printf("Container inspect error: %v\n", err)
		}
	}
}

func (a *dbeat) isExcluded(data *ContainerData) bool {
	for _, pattern := range a.config.ExcludedContainers {
		if ok, _ := regexp.MatchString(pattern, data.name); ok {
			log.Printf("The container name: %s is excluded\n", data.name)
			return true
		}
	}
	for _, pattern := range a.config.ExcludedServices {
		if ok, _ := regexp.MatchString(pattern, data.serviceName); ok {
			log.Printf("This service name: %s is excluded\n", data.serviceName)
			return true
		}
	}
	for _, pattern := range a.config.ExcludedStacks {
		if ok, _ := regexp.MatchString(pattern, data.stackName); ok {
			log.Printf("This stack name: %s is excluded\n", data.stackName)
			return true
		}
	}
	return false
}

// update ContainerData instance concidering the LogsMultiline setting
func (a *dbeat) setMultilineSetting(data *ContainerData) {
	if ml, ok := a.MLContainerMap[data.name]; ok {
		data.mlConfig = ml
		return
	}
	if ml, ok := a.MLServiceMap[data.serviceName]; ok {
		data.mlConfig = ml
		return
	}
	if ml, ok := a.MLStackMap[data.stackName]; ok {
		data.mlConfig = ml
		return
	}
	if a.MLDefault != nil {
		data.mlConfig = a.MLDefault
		return
	}
	data.mlConfig = &config.MLConfig{Activated: false}
}

func (a *dbeat) setPlainFiltersSetting(data *ContainerData) {
	if pf, ok := a.config.LogsPlainFiltersContainers[data.name]; ok {
		data.plainFilters = pf
		return
	}
	if pf, ok := a.config.LogsPlainFiltersServices[data.serviceName]; ok {
		data.plainFilters = pf
		return
	}
	if pf, ok := a.config.LogsPlainFiltersStacks[data.stackName]; ok {
		data.plainFilters = pf
		return
	}
	data.plainFilters = a.config.LogsPlainFilters
}

//Suppress a container from the main container map
func (a *dbeat) removeContainer(ID string) {
	data, ok := a.containers[ID]
	if ok {
		if data.lastLog != "" {
			a.publishEvent(data, data.lastLogTimestamp, data.lastLog)
			data.lastLog = ""
		}
		log.Println("remove container", data.name)
		delete(a.containers, ID)
	}
	err := os.Remove(path.Join(containersDateDir, ID))
	if err != nil {
		log.Println(err)
	}
}

//Update container status and health
func (a *dbeat) updateContainer(ID string) {
	data, ok := a.containers[ID]
	if ok {
		inspect, err := a.dockerClient.ContainerInspect(context.Background(), ID)
		if err == nil {
			//labels = inspect.Config.Labels
			data.state = inspect.State.Status
			data.health = ""
			if inspect.State.Health != nil {
				data.health = inspect.State.Health.Status
			}
			log.Println("update container", data.name)
		} else {
			log.Printf("Container %s inspect error: %v\n", data.name, err)
		}
	}
}

func (a *dbeat) getMapValue(labelMap map[string]string, name string) string {
	if val, exist := labelMap[name]; exist {
		return val
	}
	return ""
}

// Close dbeat ressources
func (a *dbeat) Close() {
	a.closeLogsStreams()
	a.closeMetricsStreams()
	a.dockerClient.Close()
}
