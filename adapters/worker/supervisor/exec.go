package supervisor

import (
	"errors"
	"os"
	"os/exec"
	"sort"
	"strconv"
	"strings"
	"syscall"

	"proactive-interaction-engine/internal/application/privacy"
)

const (
	providerEnvironmentPrefix = "PROACTIVE_PROVIDER_"
	providerEnvironmentCount  = providerEnvironmentPrefix + "COUNT"
	maxLaunchProviders        = 4
)

// ExecLauncher starts an explicit command in its own process group.
type ExecLauncher struct{}

func (ExecLauncher) Start(spec ProcessSpec) (Process, error) {
	if !validID(spec.ProcessID) || !validID(spec.InstanceID) {
		return nil, errors.New("worker process and instance ids are required")
	}
	providers, err := validateLaunchProviders(spec)
	if err != nil {
		return nil, err
	}
	environment, err := processEnvironment(os.Environ(), spec.Env, providers)
	if err != nil {
		return nil, err
	}
	args := append(append([]string(nil), spec.Args...), "--instance-id", spec.InstanceID)
	command := exec.Command(spec.Command, args...)
	command.Dir = spec.WorkingDir
	command.Env = environment
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	if err := command.Start(); err != nil {
		return nil, err
	}
	process := &execProcess{process: command.Process, done: make(chan error, 1)}
	go func() {
		process.done <- command.Wait()
		close(process.done)
	}()
	return process, nil
}

func validateLaunchProviders(spec ProcessSpec) ([]LogicalProviderSpec, error) {
	if len(spec.Providers) == 0 || len(spec.Providers) > maxLaunchProviders {
		return nil, errors.New("worker requires one to four logical providers")
	}
	if spec.BasePermission != privacy.CameraCapture && spec.BasePermission != privacy.MicrophoneCapture {
		return nil, errors.New("worker base permission is invalid")
	}
	providers := cloneLogicalProviders(spec.Providers)
	sort.Slice(providers, func(i, j int) bool { return providers[i].ProviderID < providers[j].ProviderID })
	hasBase := false
	for index, logical := range providers {
		if !validID(logical.ProviderID) || logical.ProviderID == spec.ProcessID {
			return nil, errors.New("logical provider id is invalid")
		}
		if !capabilityAllowed(spec.BasePermission, logical.Capability) {
			return nil, errors.New("logical provider capability is invalid for the process group")
		}
		if logical.Capability == baseCapability(spec.BasePermission) {
			hasBase = true
		}
		if index > 0 && providers[index-1].ProviderID == logical.ProviderID {
			return nil, errors.New("logical provider id is duplicated")
		}
		for previous := 0; previous < index; previous++ {
			if providers[previous].Capability == logical.Capability {
				return nil, errors.New("logical provider capability is duplicated")
			}
		}
	}
	if !hasBase {
		return nil, errors.New("worker provider subset is missing its base capability")
	}
	return providers, nil
}

func processEnvironment(parent, configured []string, providers []LogicalProviderSpec) ([]string, error) {
	output := make([]string, 0, len(parent)+len(configured)+1+2*len(providers))
	for _, entry := range parent {
		name, _, ok := strings.Cut(entry, "=")
		if ok && strings.HasPrefix(name, providerEnvironmentPrefix) {
			continue
		}
		output = append(output, entry)
	}
	configuredNames := make([]string, 0, len(configured))
	for _, entry := range configured {
		name, value, ok := strings.Cut(entry, "=")
		if !ok || !validEnvironmentName(name) || strings.IndexByte(value, 0) >= 0 {
			return nil, errors.New("worker environment entry is malformed")
		}
		if strings.HasPrefix(name, providerEnvironmentPrefix) {
			return nil, errors.New("worker environment uses a reserved provider variable")
		}
		for _, previous := range configuredNames {
			if previous == name {
				return nil, errors.New("worker environment variable is duplicated")
			}
		}
		configuredNames = append(configuredNames, name)
		output = append(output, entry)
	}
	output = append(output, providerEnvironmentCount+"="+strconv.Itoa(len(providers)))
	for index, logical := range providers {
		prefix := providerEnvironmentPrefix + strconv.Itoa(index) + "_"
		output = append(output,
			prefix+"ID="+logical.ProviderID,
			prefix+"CAPABILITY="+string(logical.Capability),
		)
	}
	return output, nil
}

func validEnvironmentName(input string) bool {
	if input == "" {
		return false
	}
	for index := 0; index < len(input); index++ {
		value := input[index]
		if (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || value == '_' || (index > 0 && value >= '0' && value <= '9') {
			continue
		}
		return false
	}
	return true
}

type execProcess struct {
	process *os.Process
	done    chan error
}

func (p *execProcess) Signal(signal os.Signal) error {
	value, ok := signal.(syscall.Signal)
	if !ok {
		return errors.New("process signal must be a syscall signal")
	}
	return syscall.Kill(-p.process.Pid, value)
}

func (p *execProcess) Kill() error {
	return syscall.Kill(-p.process.Pid, syscall.SIGKILL)
}

func (p *execProcess) Done() <-chan error { return p.done }
