package scheduler

import (
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/Dataman-Cloud/swan/src/mesosproto/mesos"
	"github.com/Dataman-Cloud/swan/src/mesosproto/sched"
	"github.com/Dataman-Cloud/swan/src/types"

	"github.com/Sirupsen/logrus"
	"github.com/golang/protobuf/proto"
)

const (
	SWAN_RESERVED_NETWORK = "swan"
)

func (s *Scheduler) BuildTask(offer *mesos.Offer, version *types.Version, name string) (*types.Task, error) {
	var task types.Task

	app, err := s.store.FetchApplication(version.AppId)
	if err != nil {
		return nil, err
	}

	if app == nil {
		return nil, fmt.Errorf("Application %s not found.", version.AppId)
	}

	task.Name = name
	if task.Name == "" {
		task.Name = fmt.Sprintf("%d.%s.%s.%s", app.Instances, app.ID, app.RunAs, app.ClusterId)

		if err := s.store.IncreaseApplicationInstances(app.ID); err != nil {
			return nil, err
		}
	}

	task.AppId = version.AppId
	task.ID = fmt.Sprintf("%d-%s", time.Now().UnixNano(), task.Name)

	task.Image = version.Container.Docker.Image
	task.Network = version.Container.Docker.Network

	if len(version.Uris) != 0 {
		task.Uris = version.Uris
	}

	if version.Container.Docker.Parameters != nil {
		for _, parameter := range version.Container.Docker.Parameters {
			task.Parameters = append(task.Parameters, &types.Parameter{
				Key:   parameter.Key,
				Value: parameter.Value,
			})
		}
	}

	// check if app run in fixed mode and has reserved enough IP
	if app.Mode == "fixed" && len(version.Ip) >= int(app.Instances) {
		task.Parameters = append(task.Parameters, &types.Parameter{
			Key:   "ip",
			Value: version.Ip[app.Instances],
		})
	}

	if version.Container.Docker.PortMappings != nil {
		for _, portMapping := range version.Container.Docker.PortMappings {
			task.PortMappings = append(task.PortMappings, &types.PortMappings{
				Port:     uint32(portMapping.ContainerPort),
				Protocol: portMapping.Protocol,
				Name:     portMapping.Name,
			})
		}
	}

	task.Privileged = version.Container.Docker.Privileged
	task.ForcePullImage = version.Container.Docker.ForcePullImage
	task.Env = version.Env
	task.Volumes = version.Container.Volumes

	if version.Labels != nil {
		task.Labels = version.Labels
	}

	task.Cpus = version.Cpus
	task.Mem = version.Mem
	task.Disk = version.Disk

	task.OfferId = *offer.GetId().Value
	task.AgentId = *offer.AgentId.Value
	task.AgentHostname = *offer.Hostname

	if version.KillPolicy != nil {
		task.KillPolicy = version.KillPolicy
	}

	if len(version.HealthChecks) > 0 {
		task.HealthChecks = version.HealthChecks
	}

	return &task, nil
}

func (s *Scheduler) BuildTaskInfo(offer *mesos.Offer, resources []*mesos.Resource, task *types.Task) *mesos.TaskInfo {
	logrus.Infof("Prepared task for launch with offer %s", *offer.GetId().Value)
	taskInfo := mesos.TaskInfo{
		Name: proto.String(task.Name),
		TaskId: &mesos.TaskID{
			Value: proto.String(task.ID),
		},
		AgentId:   offer.AgentId,
		Resources: resources,
		Command: &mesos.CommandInfo{
			Shell: proto.Bool(false),
			Value: nil,
		},
		Container: &mesos.ContainerInfo{
			Type: mesos.ContainerInfo_DOCKER.Enum(),
			Docker: &mesos.ContainerInfo_DockerInfo{
				Image: &task.Image,
			},
		},
	}

	taskInfo.Container.Docker.Privileged = &task.Privileged
	taskInfo.Container.Docker.ForcePullImage = &task.ForcePullImage

	for _, parameter := range task.Parameters {
		taskInfo.Container.Docker.Parameters = append(taskInfo.Container.Docker.Parameters, &mesos.Parameter{
			Key:   proto.String(parameter.Key),
			Value: proto.String(parameter.Value),
		})
	}

	for _, volume := range task.Volumes {
		mode := mesos.Volume_RO
		if volume.Mode == "RW" {
			mode = mesos.Volume_RW
		}
		taskInfo.Container.Volumes = append(taskInfo.Container.Volumes, &mesos.Volume{
			ContainerPath: proto.String(volume.ContainerPath),
			HostPath:      proto.String(volume.HostPath),
			Mode:          &mode,
		})
	}

	envs := make([]*mesos.Environment_Variable, 0)
	for k, v := range task.Env {
		envs = append(envs, &mesos.Environment_Variable{
			Name:  proto.String(k),
			Value: proto.String(v),
		})
	}

	taskInfo.Command.Environment = &mesos.Environment{
		Variables: envs,
	}

	uris := make([]*mesos.CommandInfo_URI, 0)
	for _, v := range task.Uris {
		uris = append(uris, &mesos.CommandInfo_URI{
			Value: proto.String(v),
		})
	}

	if len(uris) > 0 {
		taskInfo.Command.Uris = uris
	}

	if task.Labels != nil {
		labels := make([]*mesos.Label, 0)
		for k, v := range task.Labels {
			labels = append(labels, &mesos.Label{
				Key:   proto.String(k),
				Value: proto.String(v),
			})
		}

		taskInfo.Labels = &mesos.Labels{
			Labels: labels,
		}
	}

	switch task.Network {
	case "NONE":
		taskInfo.Container.Docker.Network = mesos.ContainerInfo_DockerInfo_NONE.Enum()
	case "HOST":
		taskInfo.Container.Docker.Network = mesos.ContainerInfo_DockerInfo_HOST.Enum()
	case "BRIDGE":
		ports := GetPorts(offer)
		if len(ports) == 0 {
			logrus.Errorf("No ports resource defined")
			break
		}

		for _, m := range task.PortMappings {
			hostPort := ports[s.TaskLaunched]
			taskInfo.Container.Docker.PortMappings = append(taskInfo.Container.Docker.PortMappings,
				&mesos.ContainerInfo_DockerInfo_PortMapping{
					HostPort:      proto.Uint32(uint32(hostPort)),
					ContainerPort: proto.Uint32(m.Port),
					Protocol:      proto.String(m.Protocol),
				},
			)

			taskInfo.Resources = append(taskInfo.Resources, &mesos.Resource{
				Name: proto.String("ports"),
				Type: mesos.Value_RANGES.Enum(),
				Ranges: &mesos.Value_Ranges{
					Range: []*mesos.Value_Range{
						{
							Begin: proto.Uint64(uint64(hostPort)),
							End:   proto.Uint64(uint64(hostPort)),
						},
					},
				},
			})
		}
		taskInfo.Container.Docker.Network = mesos.ContainerInfo_DockerInfo_BRIDGE.Enum()
	case SWAN_RESERVED_NETWORK:
		taskInfo.Container.Docker.Network = mesos.ContainerInfo_DockerInfo_USER.Enum()
		taskInfo.Container.NetworkInfos = append(taskInfo.Container.NetworkInfos, &mesos.NetworkInfo{
			Name: proto.String(SWAN_RESERVED_NETWORK),
		})

	default:
		taskInfo.Container.Docker.Network = mesos.ContainerInfo_DockerInfo_NONE.Enum()

	}

	if len(task.HealthChecks) > 0 {
		for _, healthCheck := range task.HealthChecks {
			if healthCheck.PortIndex < 0 || int(healthCheck.PortIndex) > len(taskInfo.Container.Docker.PortMappings) {
				healthCheck.PortIndex = 0
			}

			hostPort := proto.Uint32(0)

			for _, portMapping := range taskInfo.Container.Docker.PortMappings {
				if portMapping.ContainerPort == proto.Uint32(uint32(healthCheck.Port)) {
					hostPort = portMapping.HostPort
				}
			}

			if healthCheck.PortName != "" {
				for _, portMapping := range task.PortMappings {
					if portMapping.Name == healthCheck.PortName {
						containerPort := portMapping.Port
						for _, portMapping := range taskInfo.Container.Docker.PortMappings {
							if containerPort == *portMapping.ContainerPort {
								hostPort = portMapping.HostPort
							}
						}
					}
				}
			}

			if *hostPort == 0 {
				hostPort = taskInfo.Container.Docker.PortMappings[healthCheck.PortIndex].HostPort
			}

			protocol := strings.ToLower(healthCheck.Protocol)
			if protocol == "http" {
				taskInfo.HealthCheck = &mesos.HealthCheck{
					Type: mesos.HealthCheck_HTTP.Enum(),
					Http: &mesos.HealthCheck_HTTPCheckInfo{
						Scheme:   proto.String("http"),
						Port:     hostPort,
						Path:     &healthCheck.Path,
						Statuses: []uint32{uint32(200)},
					},
				}
			}

			if protocol == "tcp" {
				taskInfo.HealthCheck = &mesos.HealthCheck{
					Type: mesos.HealthCheck_TCP.Enum(),
					Tcp: &mesos.HealthCheck_TCPCheckInfo{
						Port: hostPort,
					},
				}
			}

			taskInfo.HealthCheck.IntervalSeconds = proto.Float64(healthCheck.IntervalSeconds)
			taskInfo.HealthCheck.TimeoutSeconds = proto.Float64(healthCheck.TimeoutSeconds)
			taskInfo.HealthCheck.ConsecutiveFailures = proto.Uint32(healthCheck.MaxConsecutiveFailures)
			taskInfo.HealthCheck.GracePeriodSeconds = proto.Float64(healthCheck.GracePeriodSeconds)
		}
	}

	return &taskInfo
}

// LaunchTasks lauch multiple tasks with specified offer.
func (s *Scheduler) LaunchTasks(offer *mesos.Offer, tasks []*mesos.TaskInfo) (*http.Response, error) {
	logrus.Infof("Launch %d tasks with offer %s", len(tasks), *offer.GetId().Value)
	call := &sched.Call{
		FrameworkId: s.framework.GetId(),
		Type:        sched.Call_ACCEPT.Enum(),
		Accept: &sched.Call_Accept{
			OfferIds: []*mesos.OfferID{
				offer.GetId(),
			},
			Operations: []*mesos.Offer_Operation{
				&mesos.Offer_Operation{
					Type: mesos.Offer_Operation_LAUNCH.Enum(),
					Launch: &mesos.Offer_Operation_Launch{
						TaskInfos: tasks,
					},
				},
			},
			Filters: &mesos.Filters{RefuseSeconds: proto.Float64(1)},
		},
	}

	logrus.Debugf("sending LaunchTasks call to mesos, the payload: %s", call.String())

	return s.send(call)
}

func (s *Scheduler) KillTask(task *types.Task) (*http.Response, error) {
	logrus.Infof("Kill task %s", task.Name)
	call := &sched.Call{
		FrameworkId: s.framework.GetId(),
		Type:        sched.Call_KILL.Enum(),
		Kill: &sched.Call_Kill{
			TaskId: &mesos.TaskID{
				Value: proto.String(task.ID),
			},
			AgentId: &mesos.AgentID{
				Value: &task.AgentId,
			},
		},
	}

	if task.KillPolicy != nil {
		if task.KillPolicy.Duration != 0 {
			call.Kill.KillPolicy = &mesos.KillPolicy{
				GracePeriod: &mesos.DurationInfo{
					Nanoseconds: proto.Int64(task.KillPolicy.Duration * 1000 * 1000),
				},
			}
		}
	}

	return s.send(call)
}