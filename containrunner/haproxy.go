package containrunner

import (
	"bytes"
	"errors"
	"fmt"
	"io/ioutil"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"
)

// Static HAProxy settings
type HAProxySettings struct {
	HAProxyBinary        string
	HAProxyConfigPath    string
	HAProxyConfigName    string
	HAProxyReloadCommand string
	HAProxySocket        string
	FirstConvergeDone    bool
}

// Dynamic HAProxy settings receivered from configbridge
type HAProxyConfiguration struct {
	Template string

	// Map file name -> file contents
	Certs map[string]string
	Files map[string]string
}

type BackendParameters struct {
	Nickname             string
	Host                 string
	HostPort             string
	Revision             string
	ServiceConfiguration ServiceConfiguration
}

type BackendParametersByNickname []BackendParameters

func (a BackendParametersByNickname) Len() int { return len(a) }
func (a BackendParametersByNickname) Swap(i, j int) {
	a[i], a[j] = a[j], a[i]
}
func (a BackendParametersByNickname) Less(i, j int) bool { return a[i].Nickname < a[j].Nickname }

type HAProxyEndpoint struct {
	Name           string
	BackendServers map[string]string `json:"-"`
}

// Log structures
type HAProxyConfigError struct {
	Config string
	Error  string
}

type HAProxyConfigChangeLog struct {
	OldConfig           string
	NewConfig           string
	OldConfigBackupFile string
}

func NewHAProxyConfiguration() *HAProxyConfiguration {
	configuration := new(HAProxyConfiguration)
	configuration.Files = make(map[string]string)
	configuration.Certs = make(map[string]string)

	return configuration
}

func NewHAProxyEndpoint() *HAProxyEndpoint {
	endpoint := new(HAProxyEndpoint)
	endpoint.BackendServers = make(map[string]string)

	return endpoint
}

func (hac *HAProxySettings) ConvergeHAProxy(configuration *RuntimeConfiguration, localInstanceInformation *LocalInstanceInformation) (error) {
	log.Debug("ConvergeHAProxy execution started")
	if configuration.MachineConfiguration.HAProxyConfiguration == nil {
		log.Warning("Warning, HAProxy config is still nil!")
		return nil
	}

	config, err := hac.BuildAndVerifyNewConfig(configuration, localInstanceInformation)
	if err != nil {
		log.Error(LogString("Error building new HAProxy configuration"))
		return err
	}

	reload_required, err := hac.UpdateBackends(configuration, localInstanceInformation)
	if err != nil {
		log.Error(LogString(fmt.Sprintf("Error updating haproxy via stats socket. Error: %+v", err)))
		return err
	}

	if !reload_required && hac.FirstConvergeDone {
		log.Debug("HAProxy could be updated without changing configuration")
		return nil
	}

	err, reload_required = hac.CommitNewConfig(config, true) // true means to do backups
	if err != nil {
		return err
	}

	if reload_required {
		err = hac.ReloadHAProxy()
		hac.FirstConvergeDone = true
	} else {
		log.Debug("ConvergeHAProxy called but reload was not required")
	}

	return err
}

func (hac *HAProxySettings) ReloadHAProxy() error {
	if hac.HAProxyReloadCommand != "" {
		log.Info("Reloading haproxy with " + hac.HAProxyReloadCommand)
		parts := strings.Fields(hac.HAProxyReloadCommand)
		head := parts[0]
		parts = parts[1:len(parts)]

		cmd := exec.Command(head, parts...)
		err := cmd.Start()
		if err != nil {
			panic(err)
		}

		err = cmd.Wait()
		return err

	} else {
		log.Debug("Tried to reload haproxy but no reload command set!")
	}
	return nil
}

func (hac *HAProxySettings) BuildAndVerifyNewConfig(configuration *RuntimeConfiguration, localInstanceInformation *LocalInstanceInformation) (string, error) {

	new_config, err := ioutil.TempFile(os.TempDir(), "haproxy_new_config_")
	if new_config != nil {
		defer os.Remove(new_config.Name())
	} else {
		fmt.Fprintf(os.Stderr, "Error: new_config was nil when creating temp file. Err: %+v\n", err)
	}

	config, err := hac.GetNewConfig(configuration, localInstanceInformation)
	if err != nil {
		return "", err
	}

	new_config.WriteString(config)
	new_config.Close()

	_, err = os.Stat(hac.HAProxyConfigPath + "/certs.d")
	if err != nil || os.IsNotExist(err) {
		err := os.Mkdir(hac.HAProxyConfigPath+"/certs.d", 0644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error: Could not create directory for haproxy certs. Err: %+v\n", err)
			return "", err
		}
	}

	if configuration.MachineConfiguration.HAProxyConfiguration.Certs != nil {
		for name, contents := range configuration.MachineConfiguration.HAProxyConfiguration.Certs {
			fname := hac.HAProxyConfigPath + "/certs.d/" + name
			//fmt.Fprintf(os.Stderr, "Writing haproxy file %s\n", fname)

			err := ioutil.WriteFile(fname, []byte(contents), 0644)
			if err != nil {
				panic(err)
			}
		}
	}

	if configuration.MachineConfiguration.HAProxyConfiguration.Files != nil {
		for name, contents := range configuration.MachineConfiguration.HAProxyConfiguration.Files {
			fname := hac.HAProxyConfigPath + "/" + name
			err := ioutil.WriteFile(fname, []byte(contents), 0644)
			if err != nil {
				panic(err)
			}
		}
	}

	cmd := exec.Command(hac.HAProxyBinary, "-c", "-f", new_config.Name())
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error (cmd.StderrPipe) verifying haproxy config with binary %s. Error: %+v\n", hac.HAProxyBinary, err)
		return "", err
	}

	if err := cmd.Start(); err != nil {
		fmt.Fprintf(os.Stderr, "Error (cmd.Start) verifying haproxy config with binary %s. Error: %+v\n", hac.HAProxyBinary, err)
		return "", err
	}

	stderr, err := ioutil.ReadAll(stderrPipe)
	err = cmd.Wait()

	if err != nil {
		log.Error(LogEvent(HAProxyConfigError{config, string(stderr)}))
		return "", errors.New("Invalid HAProxy configuration")
	}

	return config, nil
}

func (hac *HAProxySettings) CommitNewConfig(config string, backup bool) (error, bool) {

	l := HAProxyConfigChangeLog{}
	var contents []byte
	contents, err := ioutil.ReadFile(hac.HAProxyConfigPath + "/" + hac.HAProxyConfigName)
	if err == nil {
		l.OldConfig = string(contents)
	}

	l.NewConfig = config

	if config == string(contents) {
		log.Debug("CommitNewConfig determined that HAProxy configuration file has not changed")
		return nil, false
	} 

	log.Info("HAProxy configuration contents has changed, so writing a new file to %s", hac.HAProxyConfigPath + "/" + hac.HAProxyConfigName)


	if backup {
		l.OldConfigBackupFile = hac.HAProxyConfigPath + "/" + hac.HAProxyConfigName + "-" + time.Now().Format(time.RFC3339)

		err = os.Link(hac.HAProxyConfigPath+"/"+hac.HAProxyConfigName, l.OldConfigBackupFile)
		if err != nil && !os.IsNotExist(err) {
			log.Error(LogString("Error linking config backup!" + err.Error()))
			return err, false
		} else if err != nil && os.IsNotExist(err) {
			l.OldConfigBackupFile = ""
		}
	}

	//log.Debug(LogEvent(l))

	err = ioutil.WriteFile(hac.HAProxyConfigPath+"/"+hac.HAProxyConfigName, []byte(config), 0664)
	if err != nil {
		log.Error(LogString("Could not write new haproxy config!" + err.Error()))
		return err, false
	}

	mtime := time.Now().Local()
	os.Chtimes(hac.HAProxyConfigPath+"/haproxy-lastupdated.txt", mtime, mtime)

	return nil, true

}

func (hac *HAProxySettings) GetNewConfig(configuration *RuntimeConfiguration, localInstanceInformation *LocalInstanceInformation) (string, error) {

	funcMap := template.FuncMap{
		// The name "title" is what the function will be called in the template text.
		"Endpoints": func(service_name string) ([]BackendParameters, error) {
			var backends []BackendParameters
			backend_servers, ok := configuration.ServiceBackends[service_name]

			if ok {
				for hostport, endpointInfo := range backend_servers {
					backends = append(backends, BackendParameters{
						Nickname:             service_name + "-" + hostport,
						Host:                 strings.Split(hostport, ":")[0],
						HostPort:             hostport,
						Revision:             endpointInfo.Revision,
						ServiceConfiguration: endpointInfo.ServiceConfiguration,
					})
				}

				sort.Sort(BackendParametersByNickname(backends))
			}

			localInstanceInformation.LocallyRequiredServices[service_name] = backend_servers

			return backends, nil
		},
		"LocalEndpoints": func(service_name string) ([]BackendParameters, error) {
			var backends []BackendParameters
			backend_servers, ok := configuration.ServiceBackends[service_name]

			if ok {
				for hostport, endpointInfo := range backend_servers {
					if endpointInfo != nil && endpointInfo.AvailabilityZone == localInstanceInformation.AvailabilityZone {
						backends = append(backends, BackendParameters{
							Nickname:             service_name + "-" + hostport,
							Host:                 strings.Split(hostport, ":")[0],
							HostPort:             hostport,
							Revision:             endpointInfo.Revision,
							ServiceConfiguration: endpointInfo.ServiceConfiguration,
						})
					}
				}

				sort.Sort(BackendParametersByNickname(backends))
			}

			localInstanceInformation.LocallyRequiredServices[service_name] = backend_servers

			return backends, nil
		},
	}

	tmpl, err := template.New("main").Funcs(funcMap).Parse(configuration.MachineConfiguration.HAProxyConfiguration.Template)
	if err != nil {
		log.Error("parsing: %s", err)
		return "", err
	}

	output := new(bytes.Buffer)
	// Run the template to verify the output.
	err = tmpl.Execute(output, "the go programming language")
	if err != nil {
		log.Error("execution: %s", err)
		return "", err
	}

	return output.String(), nil
}

func runHAProxyCommand(command string, socket_name string) error {
	c, err := net.Dial("unix", socket_name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening HAProxy socket. Error: %+v\n", err)
		if c != nil {
			c.Close()
		}
		return err
	}
	defer c.Close()

	_, err = c.Write([]byte(command))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error on stat command '%s'. Error: %+v\n", command, err)
		return err
	}

	//fmt.Fprintf(os.Stderr, "running command to socket %s. Command: %s", socket_name, command)

	return nil
}

func (hac *HAProxySettings) GetHaproxyBackends() (current_backends map[string]map[string]string, err error) {
	sockets, err := filepath.Glob(hac.HAProxySocket)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error finding haproxy sockets from path %s: %+v\n", hac.HAProxySocket, err)
		return nil, err
	}

	if len(sockets) == 0 {
		fmt.Fprintf(os.Stderr, "Could not find haproxy socket(s) from %s\n", hac.HAProxySocket)
		return nil, nil
	}

	c, err := net.Dial("unix", sockets[0])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error opening HAProxy socket. Error: %+v\n", err)
		if c != nil {
			c.Close()
		}
		return nil, nil
	}
	defer c.Close()

	_, err = c.Write([]byte("show stat\n"))
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error on show stat command. Error: %+v\n", err)
		return nil, nil
	}

	var bytes []byte
	bytes, err = ioutil.ReadAll(c)
	lines := strings.Split(string(bytes), "\n")

	c.Close()

	// Build list of currently existing backends in the running haproxy
	current_backends = make(map[string]map[string]string)

	for _, line := range lines {
		if line == "" || line[0] == '#' {
			continue
		}
		parts := strings.Split(line, ",")
		//fmt.Printf("Read line: %+v\n", line)
		if parts[1] == "FRONTEND" || parts[1] == "BACKEND" {
			continue
		}

		if _, ok := current_backends[parts[0]]; ok == false {
			current_backends[parts[0]] = make(map[string]string)
		}
		current_backends[parts[0]][parts[1]] = parts[17]
	}

	return current_backends, err
}

func (hac *HAProxySettings) UpdateBackends(configuration *RuntimeConfiguration, localInstanceInformation *LocalInstanceInformation) (bool, error) {

	current_backends, err := hac.GetHaproxyBackends()
	if err != nil {
		return true, nil
	}

	commands := ""

	enabled_backends := make(map[string]bool)
	total_backends := 0

	//fmt.Printf("current backends: %+v\n", current_backends)

	fmt.Printf("LocallyRequiredServices: %+v\n", localInstanceInformation.LocallyRequiredServices)

	for service_name, backend_servers := range localInstanceInformation.LocallyRequiredServices {
		fmt.Printf("Service backend for service_name %s: %+v", service_name, backend_servers)
		// Check that there actually is configured servers for this backend before dooming that haproxy needs to be restarted
		if _, ok := current_backends[service_name]; ok == false && len(backend_servers) > 0 {
			fmt.Printf("Restart required: missing section %s. Notice that the backend name must match the individual endpoint names.\n", service_name)
			//fmt.Printf("current backends: %+v\n", current_backends)
			//fmt.Printf("locally required services: %+v\n", configuration.LocallyRequiredServices)
			return true, nil
		}
		for backendServer := range backend_servers {
			if _, ok := current_backends[service_name][service_name+"-"+backendServer]; ok == false {
				fmt.Printf("Restart required: missing endpoint %s from section %s\n", service_name+"-"+backendServer, service_name)
				return true, nil
			}
			enabled_backends[service_name+"-"+backendServer] = true
		}
	}
	//fmt.Printf("enabled backends: %+v\n", enabled_backends)
	if len(enabled_backends) == 0 {
		fmt.Printf("No enabled backends, will not disable anything\n")
		return true, nil
	}

	for section_name, section_backends := range current_backends {
		for backend, backend_status := range section_backends {
			total_backends++
			command := ""
			if _, ok := enabled_backends[backend]; ok == true {
				if backend_status == "MAINT" {
					command = "enable server " + section_name + "/" + backend + "\n"
				}
			} else if strings.Index(backend, "nocheck-") == -1 { // having "nocheck-" prefix on backend server name prevents orbit from disabling the backend
				if backend_status != "MAINT" {
					command = "disable server " + section_name + "/" + backend + "\n"
				}
			}

			if command == "" {
				continue
			}

			log.Debug(fmt.Sprintf("executing command: %s", command))

			commands += command
		}
	}

	if len(commands) > 0 {

		sockets, err := filepath.Glob(hac.HAProxySocket)
		if err != nil {
			fmt.Fprintf(os.Stderr, "Error finding haproxy sockets from path %s: %+v\n", hac.HAProxySocket, err)
		}

		fmt.Printf("Running these haproxy commands to %d sockets:\n%s\n", len(sockets), commands)

		for _, socket_name := range sockets {
			runHAProxyCommand(commands, socket_name)
			if err != nil {
				return true, err
			}
		}

		err = ioutil.WriteFile(hac.HAProxyConfigPath+"/haproxy-lastupdated.txt", []byte(commands), 0664)
		if err != nil {
			log.Error("Could not update haproxy-lastupdated file due to error: %+v", err)
		}		
	}

	return false, nil
}
