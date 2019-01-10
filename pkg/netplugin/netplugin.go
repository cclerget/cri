// Copyright (c) 2019 Sylabs, Inc. All rights reserved.
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

package netplugin

import (
	"k8s.io/kubernetes/pkg/kubelet/dockershim/network"
	"k8s.io/kubernetes/pkg/kubelet/dockershim/network/cni"
)

const (
	CNIBinDir  = "/opt/cni/bin"
	CNIConfDir = "/etc/cni/net.d"
)

// Get initialize network plugin
func Get(confDir string, binDirs []string) network.NetworkPlugin {
	cDir := confDir
	bDirs := binDirs

	if cDir == "" {
		cDir = CNIConfDir
	}

	if len(bDirs) == 0 {
		bDirs = []string{CNIBinDir}
	}

	return cni.ProbeNetworkPlugins(cDir, bDirs)[0]
}
