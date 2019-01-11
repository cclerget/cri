// Copyright (c) 2018 Sylabs, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runtime

import (
	"context"
	"fmt"

	specs "github.com/opencontainers/runtime-spec/specs-go"

	v1 "k8s.io/api/core/v1"
	kubecontainer "k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/dockershim/network/hostport"

	"github.com/sylabs/cri/pkg/index"
	"github.com/sylabs/cri/pkg/kube"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	glog "k8s.io/klog"
	k8s "k8s.io/kubernetes/pkg/kubelet/apis/cri/runtime/v1alpha2"
)

// RunPodSandbox creates and starts a pod-level sandbox. Runtimes must ensure
// the sandbox is in the ready state on success.
func (s *SingularityRuntime) RunPodSandbox(_ context.Context, req *k8s.RunPodSandboxRequest) (*k8s.RunPodSandboxResponse, error) {
	config := req.Config
	pod := kube.NewPod(config)
	cleanupOnFailure := func() {
		if err := s.pods.Remove(pod.ID()); err != nil {
			glog.Errorf("Could not remove pod from index: %v", err)
		}
	}
	if err := pod.Run(); err != nil {
		cleanupOnFailure()
		return nil, status.Errorf(codes.Internal, "could not run pod: %v", err)
	}
	err := s.pods.Add(pod)
	if err != nil {
		cleanupOnFailure()
		return nil, err
	}

	if pod.NamespacePath(specs.NetworkNamespace) != "" {
		// setup POD network
		containerID := kubecontainer.BuildContainerID("singularity", pod.ID())
		err = s.networkPlugin.SetUpPod(config.GetMetadata().Namespace, config.GetMetadata().Name, containerID, config.Annotations, nil)
		if err != nil {
			// ignore error
			s.networkPlugin.TearDownPod(config.GetMetadata().Namespace, config.GetMetadata().Name, containerID)

			cleanupOnFailure()
			return nil, err
		}
	}

	return &k8s.RunPodSandboxResponse{
		PodSandboxId: pod.ID(),
	}, nil
}

// StopPodSandbox stops any running process that is part of the sandbox and
// reclaims network resources (e.g., IP addresses) allocated to the sandbox.
// If there are any running containers in the sandbox, they must be forcibly
// terminated.
// This call is idempotent, and must not return an error if all relevant
// resources have already been reclaimed. kubelet will call StopPodSandbox
// at least once before calling RemovePodSandbox. It will also attempt to
// reclaim resources eagerly, as soon as a sandbox is not needed. Hence,
// multiple StopPodSandbox calls are expected.
func (s *SingularityRuntime) StopPodSandbox(_ context.Context, req *k8s.StopPodSandboxRequest) (*k8s.StopPodSandboxResponse, error) {
	pod, err := s.findPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}

	if pod.NamespacePath(specs.NetworkNamespace) != "" {
		containerID := kubecontainer.BuildContainerID("singularity", pod.ID())
		err = s.networkPlugin.TearDownPod(pod.GetMetadata().Namespace, pod.GetMetadata().Name, containerID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "could not tear down pod network interface: %v", err)
		}
	}

	if err := pod.Stop(); err != nil {
		return nil, status.Errorf(codes.Internal, "could not stop pod: %v", err)
	}
	return &k8s.StopPodSandboxResponse{}, nil
}

// RemovePodSandbox removes the sandbox. If there are any running containers
// in the sandbox, they must be forcibly terminated and removed.
// This call is idempotent, and must not return an error if the sandbox has
// already been removed.
func (s *SingularityRuntime) RemovePodSandbox(_ context.Context, req *k8s.RemovePodSandboxRequest) (*k8s.RemovePodSandboxResponse, error) {
	pod, err := s.pods.Find(req.PodSandboxId)
	if err == index.ErrPodNotFound {
		return &k8s.RemovePodSandboxResponse{}, nil
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	containers := pod.Containers() // save container IDs to cleanup index later
	if err := pod.Remove(); err != nil {
		return nil, status.Errorf(codes.Internal, "could not remove pod: %v", err)
	}
	if err := s.pods.Remove(pod.ID()); err != nil {
		return nil, status.Errorf(codes.Internal, "could not remove pod from index: %v", err)
	}
	for _, containerID := range containers {
		if err := s.containers.Remove(containerID); err != nil {
			return nil, status.Errorf(codes.Internal, "could not remove container from index: %v", err)
		}
	}
	return &k8s.RemovePodSandboxResponse{}, nil
}

// PodSandboxStatus returns the status of the PodSandbox.
// If the PodSandbox is not present, returns an error.
func (s *SingularityRuntime) PodSandboxStatus(_ context.Context, req *k8s.PodSandboxStatusRequest) (*k8s.PodSandboxStatusResponse, error) {
	pod, err := s.findPod(req.PodSandboxId)
	if err != nil {
		return nil, err
	}
	if err := pod.UpdateState(); err != nil {
		return nil, status.Errorf(codes.Internal, "could not update pod state: %v", err)
	}

	var verboseInfo map[string]string
	if req.Verbose {
		verboseInfo = map[string]string{
			"pid": fmt.Sprintf("%d", pod.Pid()),
		}
	}

	var networkStatus *k8s.PodSandboxNetworkStatus

	if pod.NamespacePath(specs.NetworkNamespace) != "" {
		containerID := kubecontainer.BuildContainerID("singularity", pod.ID())
		netPodStatus, err := s.networkPlugin.GetPodNetworkStatus(pod.GetMetadata().Namespace, pod.GetMetadata().Name, containerID)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "could not retrieve pod network status: %v", err)
		}
		networkStatus = &k8s.PodSandboxNetworkStatus{Ip: netPodStatus.IP.String()}
	}

	return &k8s.PodSandboxStatusResponse{
		Status: &k8s.PodSandboxStatus{
			Id:        pod.ID(),
			Metadata:  pod.GetMetadata(),
			State:     pod.State(),
			CreatedAt: pod.CreatedAt(),
			Network:   networkStatus,
			Linux: &k8s.LinuxPodSandboxStatus{
				Namespaces: &k8s.Namespace{
					Options: pod.GetLinux().GetSecurityContext().GetNamespaceOptions(),
				},
			},
			Labels:      pod.GetLabels(),
			Annotations: pod.GetAnnotations(),
		},
		Info: verboseInfo,
	}, nil
}

// ListPodSandbox returns a list of PodSandboxes.
func (s *SingularityRuntime) ListPodSandbox(_ context.Context, req *k8s.ListPodSandboxRequest) (*k8s.ListPodSandboxResponse, error) {
	var pods []*k8s.PodSandbox

	appendPodToResult := func(pod *kube.Pod) {
		if err := pod.UpdateState(); err != nil {
			glog.Errorf("Could not update pod state: %v", err)
			return
		}
		if pod.MatchesFilter(req.Filter) {
			pods = append(pods, &k8s.PodSandbox{
				Id:          pod.ID(),
				Metadata:    pod.GetMetadata(),
				State:       pod.State(),
				CreatedAt:   pod.CreatedAt(),
				Labels:      pod.GetLabels(),
				Annotations: pod.GetAnnotations(),
			})
		}
	}
	s.pods.Iterate(appendPodToResult)
	return &k8s.ListPodSandboxResponse{
		Items: pods,
	}, nil
}

// GetPodPortMappings returns configured port mappings for
// a pod sandbox.
func (s *SingularityRuntime) GetPodPortMappings(containerID string) ([]*hostport.PortMapping, error) {
	pod, err := s.findPod(containerID)
	if err != nil {
		return nil, err
	}

	portMappings := make([]*hostport.PortMapping, 0)

	for _, pm := range pod.PodSandboxConfig.GetPortMappings() {
		portMappings = append(portMappings, &hostport.PortMapping{
			HostPort:      pm.HostPort,
			ContainerPort: pm.ContainerPort,
			Protocol:      v1.Protocol(pm.Protocol),
			HostIP:        pm.HostIp,
		})
	}

	return portMappings, nil
}

// GetNetNS returns path to network namespace for a pod sandbox.
func (s *SingularityRuntime) GetNetNS(containerID string) (string, error) {
	pod, err := s.findPod(containerID)
	if err != nil {
		return "", err
	}

	path := pod.NamespacePath(specs.NetworkNamespace)
	if path == "" {
		return "", fmt.Errorf("can't determine network namespace path")
	}

	return pod.NamespacePath(specs.NetworkNamespace), nil
}

func (s *SingularityRuntime) findPod(id string) (*kube.Pod, error) {
	pod, err := s.pods.Find(id)
	if err == index.ErrPodNotFound {
		return nil, status.Errorf(codes.NotFound, "pod is not found")
	}
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	return pod, nil
}
