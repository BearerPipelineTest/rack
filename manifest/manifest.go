package manifest

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"io/ioutil"
	"net"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
)

type Manifest struct {
	Version string `yaml:"version"`

	Services map[string]Service `yaml:"services"`
}

// Load a Manifest from raw data
func Load(data []byte) (*Manifest, error) {
	v, err := manifestVersion(data)

	if err != nil {
		return nil, err
	}

	m := &Manifest{Version: v}

	switch v {
	case "1":
		if err := yaml.Unmarshal(data, &m.Services); err != nil {
			return nil, fmt.Errorf("error loading manifest: %s", err)
		}
	case "2":
		if err := yaml.Unmarshal(data, m); err != nil {
			return nil, fmt.Errorf("error loading manifest: %s", err)
		}
	default:
		return nil, fmt.Errorf("unknown manifest version: %s", v)
	}

	for name, service := range m.Services {
		service.Name = name
		m.Services[name] = service
	}

	err = m.Validate()
	if err != nil {
		return nil, err
	}

	return m, nil
}

// Load a Manifest from a file
func LoadFile(path string) (*Manifest, error) {
	data, err := ioutil.ReadFile(path)

	if err != nil {
		return nil, err
	}

	return Load(data)
}

func (m Manifest) Validate() error {
	regexValidCronLabel := regexp.MustCompile(`\A[a-zA-Z][-a-zA-Z0-9]{3,29}\z`)

	for _, entry := range m.Services {
		labels := entry.LabelsByPrefix("convox.cron")
		for k, _ := range labels {
			parts := strings.Split(k, ".")
			if len(parts) != 3 {
				return fmt.Errorf(
					"Cron task is not valid (must be in format convox.cron.myjob)",
				)
			}
			name := parts[2]
			if !regexValidCronLabel.MatchString(name) {
				return fmt.Errorf(
					"Cron task %s is not valid (cron names can contain only alphanumeric characters and dashes and must be between 4 and 30 characters)",
					name,
				)
			}
		}
	}
	return nil
}

// Return a list of ports this manifest will expose when run
func (m *Manifest) ExternalPorts() []int {
	ext := []int{}

	for _, service := range m.Services {
		for _, port := range service.Ports {
			if port.External() {
				ext = append(ext, port.Balancer)
			}
		}
	}

	return ext
}

// Find any port conflits that would prevent this manifest from running
func (m *Manifest) PortConflicts() ([]int, error) {
	ext := m.ExternalPorts()

	conflicts := make([]int, 0)

	host := dockerHost()

	for _, p := range ext {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, p), 200*time.Millisecond)

		if err == nil {
			conflicts = append(conflicts, p)
			defer conn.Close()
		}
	}

	sort.Ints(conflicts)

	return conflicts, nil
}

func (m *Manifest) Run(dir, app string, noCache bool) Run {
	return NewRun(dir, app, *m, noCache)
}

// Return the Services of this Manifest in the order you should run them
func (m *Manifest) runOrder() []Service {
	services := []Service{}

	for _, service := range m.Services {
		services = append(services, service)
	}

	// classic bubble sort
	for i := 0; i < len(services)-1; i++ {
		for j := i + 1; j < len(services); j++ {
			// swap if j is a dependency of i
			for _, name := range services[i].Links {
				if name == services[j].Name {
					services[i], services[j] = services[j], services[i]
					break
				}
			}
		}
	}

	return services
}

// Shift all external ports in this Manifest by the given amount
func (m *Manifest) Shift(shift int) {
	for _, service := range m.Services {
		service.Ports.Shift(shift)
	}
}

func manifestPrefix(m Manifest, prefix string) string {
	max := 6

	for name, _ := range m.Services {
		if len(name) > max {
			max = len(name)
		}
	}

	return fmt.Sprintf(fmt.Sprintf("%%-%ds |", max), prefix)
}

func systemPrefix(m *Manifest) string {
	return manifestPrefix(*m, "convox")
}

func manifestVersion(data []byte) (string, error) {
	var check struct {
		Version string
	}

	if err := yaml.Unmarshal(data, &check); err != nil {
		return "", fmt.Errorf("could not parse manifest: %s", err)
	}

	if check.Version != "" {
		return check.Version, nil
	}

	return "1", nil
}

func buildTemplate(name, section string, input interface{}) (string, error) {
	data, err := Asset(fmt.Sprintf("templates/%s.tmpl", name))

	if err != nil {
		return "", err
	}

	tmpl, err := template.New(section).Funcs(templateHelpers()).Parse(string(data))

	if err != nil {
		return "", err
	}

	var formation bytes.Buffer

	err = tmpl.Execute(&formation, input)

	if err != nil {
		return "", err
	}

	return formation.String(), nil
}

func prettyJson(raw string) (string, error) {
	var parsed map[string]interface{}

	if err := json.Unmarshal([]byte(raw), &parsed); err != nil {
		if syntax, ok := err.(*json.SyntaxError); ok {
			lines := strings.Split(raw, "\n")
			lineno := len(strings.Split(raw[0:syntax.Offset], "\n")) - 1
			start := lineno - 3
			end := lineno + 3
			output := "\n"

			if start < 0 {
				start = 0
			}

			if end >= len(lines) {
				end = len(lines) - 1
			}

			for i := start; i <= end; i++ {
				output += fmt.Sprintf("%03d: %s\n", i, lines[i])
			}

			output += err.Error()

			return "", fmt.Errorf(output)
		}

		return "", err
	}

	bp, err := json.MarshalIndent(parsed, "", "  ")

	if err != nil {
		return "", err
	}

	return string(bp), nil
}

func templateHelpers() template.FuncMap {
	return template.FuncMap{
		"upper": func(s string) string {
			return UpperName(s)
		},
		"value": func(s string) template.HTML {
			return template.HTML(fmt.Sprintf("%q", s))
		},
	}
}

func (m *Manifest) Raw() ([]byte, error) {
	return yaml.Marshal(m)
}
