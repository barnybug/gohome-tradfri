// Service to communicate with ikea tradfri hardware via the gateway.
package main

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
	"strings"
	"time"

	yaml "gopkg.in/yaml.v1"

	tradfri "github.com/barnybug/go-tradfri"
	"github.com/barnybug/gohome/pubsub"
	"github.com/barnybug/gohome/services"
	"github.com/edgard/yeelight"
)

// Service tradfri
type Service struct {
	lights  map[string]*yeelight.Light
	config  Config
	client  *tradfri.Client
	devices map[int]*tradfri.DeviceDescription
	groups  map[int]*tradfri.GroupDescription
}

func (self *Service) ID() string {
	return "tradfri"
}

// Config tradfri
type Config struct {
	Tradfri struct {
		Gateway string
		Key     string
	}
}

var reHexCode = regexp.MustCompile(`^#[0-9a-f]{6}$`)

func (self *Service) handleCommand(ev *pubsub.Event) {
	dev := ev.Device()
	pids := services.Config.LookupDeviceProtocol(dev)
	if pids["tradfri"] == "" {
		return // command not for us
	}
	command := ev.Command()
	if command != "off" && command != "on" {
		log.Println("Command not recognised:", command)
		return
	}

	var id int
	group := false
	if strings.HasPrefix(pids["tradfri"], "G") {
		group = true
		id, _ = strconv.Atoi(pids["tradfri"][1:])
	} else {
		id, _ = strconv.Atoi(pids["tradfri"])
	}

	var s string

	ms := 500
	if _, ok := ev.Fields["duration"]; ok {
		ms = int(ev.IntField("duration"))
	}
	duration := tradfri.MsToDuration(ms)

	switch ev.Command() {
	case "on":
		s = "on"
		power := 1
		change := tradfri.LightControl{Power: &power}
		level := int(ev.IntField("level"))
		if level != 0 {
			dim := tradfri.PercentageToDim(level)
			change.Dim = &dim
			s += fmt.Sprintf(" level %d%%", level)
		}
		colour := ev.StringField("colour")
		if reHexCode.MatchString(colour) {
			colour = colour[1:]
			change.Color = &colour
			s += fmt.Sprintf(" colour %s", colour)
		}
		temp := int(ev.IntField("temp"))
		if temp != 0 {
			mired := tradfri.KelvinToMired(temp)
			change.Mireds = &mired
			s += fmt.Sprintf(" temp %dK", temp)
		}
		change.Duration = &duration
		if group {
			self.client.SetGroup(id, change)
		} else {
			self.client.SetDevice(id, change)
		}
	case "off":
		s = "off"
		power := 0
		change := tradfri.LightControl{Power: &power}
		change.Duration = &duration
		if group {
			self.client.SetGroup(id, change)
		} else {
			self.client.SetDevice(id, change)
		}
	}

	if group {
		log.Printf("Setting group %d to %s\n", id, s)
		d, _ := self.client.GetGroupDescription(id)
		fields := pubsub.Fields{
			"device":  dev,
			"command": command,
			"level":   d.Dim,
		}
		ack := pubsub.NewEvent("ack", fields)
		services.Publisher.Emit(ack)
	} else {
		log.Printf("Setting device %d to %s\n", id, s)
		d, _ := self.client.GetDeviceDescription(id)
		fields := pubsub.Fields{
			"device":  dev,
			"command": command,
			"level":   tradfri.DimToPercentage(*d.LightControl[0].Dim),
			"temp":    tradfri.MiredToKelvin(*d.LightControl[0].Mireds),
		}
		ack := pubsub.NewEvent("ack", fields)
		services.Publisher.Emit(ack)
	}
}

func deviceSource(device *tradfri.DeviceDescription) string {
	return fmt.Sprintf("tradfri.%d", device.DeviceID)
}

func groupSource(group *tradfri.GroupDescription) string {
	return fmt.Sprintf("tradfri.G%d", group.GroupID)
}

func announce(source, name string) {
	fields := pubsub.Fields{
		"source": source,
		"name":   name,
	}
	ev := pubsub.NewEvent("announce", fields)
	services.Config.AddDeviceToEvent(ev)
	services.Publisher.Emit(ev)
}

func (self *Service) discover() (int, int) {
	devices, err := self.client.ListDevices()
	if err != nil {
		log.Fatal(err)
	}

	for _, device := range devices {
		self.devices[device.DeviceID] = device
		source := deviceSource(device)
		log.Printf("Announcing device: %s %s", source, device.DeviceName)
		announce(source, device.DeviceName)
	}

	groups, err := self.client.ListGroups()
	if err != nil {
		log.Fatal(err)
	}

	for _, group := range groups {
		self.groups[group.GroupID] = group
		source := groupSource(group)
		if _, ok := services.Config.LookupSource(source); !ok {
			log.Printf("Announcing new group discovered: %s %s", source, group.GroupName)
			announce(source, group.GroupName)
		}
	}
	return len(devices), len(groups)
}

func (self *Service) QueryHandlers() services.QueryHandlers {
	return services.QueryHandlers{
		"discover": services.TextHandler(self.queryDiscover),
		"help":     services.StaticHandler("discover: run discovery\n"),
	}
}

func (self *Service) queryDiscover(q services.Question) string {
	devices, groups := self.discover()
	return fmt.Sprintf("Discovered %d devices, %d groups", devices, groups)
}

func (self *Service) loadConfig() error {
	err := yaml.Unmarshal(services.RawConfig, &self.config)
	return err
}

func (self *Service) Run() error {
	if err := self.loadConfig(); err != nil {
		return err
	}

	self.client = tradfri.NewClient(self.config.Tradfri.Gateway)
	tradfri.SetDebug(false)
	if err := self.client.LoadPSK(); err != nil {
		// key required
		self.client.Key = self.config.Tradfri.Key
	}
	if err := self.client.Connect(); err != nil {
		return err
	}
	self.client.SavePSK()

	commandChannel := services.Subscriber.FilteredChannel("command")
	self.devices = map[int]*tradfri.DeviceDescription{}
	self.groups = map[int]*tradfri.GroupDescription{}
	self.discover()
	// Rescan for new devices every hour
	autoDiscover := time.Tick(60 * time.Minute)

	for {
		select {
		case <-autoDiscover:
			self.discover()

		case command := <-commandChannel:
			self.handleCommand(command)
		}
	}
}

func main() {
	services.Setup()
	services.Register(&Service{})
	services.Launch([]string{"tradfri"})
}
