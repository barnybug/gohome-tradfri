// Service to communicate with ikea tradfri hardware via the gateway.
package main

import (
	"fmt"
	"log"
	"regexp"
	"strconv"
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

	id, _ := strconv.Atoi(pids["tradfri"])
	group := id&(1<<17) != 0
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
		groupAck(d)
		self.groups[id] = d
	} else {
		log.Printf("Setting device %d to %s\n", id, s)
		d, _ := self.client.GetDeviceDescription(id)
		deviceAck(d)
		self.devices[id] = d
	}
}

func numCommand(i int) string {
	if i == 1 {
		return "on"
	}
	return "off"
}

func deviceSource(device *tradfri.DeviceDescription) string {
	return fmt.Sprintf("tradfri.%d", device.DeviceID)
}

func groupSource(group *tradfri.GroupDescription) string {
	return fmt.Sprintf("tradfri.%d", group.GroupID)
}

func deviceAck(d *tradfri.DeviceDescription) {
	lc := d.LightControl[0]
	fields := pubsub.Fields{
		"source":  deviceSource(d),
		"command": numCommand(*lc.Power),
		"level":   tradfri.DimToPercentage(*lc.Dim),
		"temp":    tradfri.MiredToKelvin(*lc.Mireds),
	}
	ack := pubsub.NewEvent("ack", fields)
	services.Config.AddDeviceToEvent(ack)
	services.Publisher.Emit(ack)
}

func groupAck(d *tradfri.GroupDescription) {
	fields := pubsub.Fields{
		"source":  groupSource(d),
		"command": numCommand(d.Power),
		"level":   d.Dim,
	}
	ack := pubsub.NewEvent("ack", fields)
	services.Config.AddDeviceToEvent(ack)
	services.Publisher.Emit(ack)
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

func deviceChanged(a *tradfri.DeviceDescription, b *tradfri.DeviceDescription) bool {
	if len(a.LightControl) == 0 || len(b.LightControl) == 0 {
		// remote control
		return false
	}
	la := a.LightControl[0]
	lb := b.LightControl[0]
	return *la.Power != *lb.Power || *la.Dim != *lb.Dim || *la.Mireds != *lb.Mireds
}

func (self *Service) discover() (int, int) {
	devices := map[int]*tradfri.DeviceDescription{}
	groups := map[int]*tradfri.GroupDescription{}

	devicesList, err := self.client.ListDevices()
	if err != nil {
		log.Fatal(err)
	}

	for _, device := range devicesList {
		devices[device.DeviceID] = device
		source := deviceSource(device)
		announce(source, device.DeviceName)
		if d, ok := self.devices[device.DeviceID]; ok && deviceChanged(d, device) {
			// update device state if changed externally (ie remote)
			log.Printf("Device %s changed", source)
			deviceAck(device)
		}
	}

	groupsList, err := self.client.ListGroups()
	if err != nil {
		log.Fatal(err)
	}

	for _, group := range groupsList {
		groups[group.GroupID] = group
		source := groupSource(group)
		if _, ok := services.Config.LookupSource(source); !ok {
			announce(source, group.GroupName)
		}
	}

	self.devices = devices
	self.groups = groups

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
	self.discover()
	// Rescan for new devices every 5 minutes
	autoDiscover := time.Tick(5 * time.Minute)

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
