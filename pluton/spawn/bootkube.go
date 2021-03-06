// Copyright 2017 CoreOS, Inc.
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

package spawn

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"path/filepath"
	"regexp"
	"strings"
	"text/template"
	"time"

	"github.com/coreos/mantle/kola/cluster"
	"github.com/coreos/mantle/kola/tests/etcd"
	"github.com/coreos/mantle/platform"
	"github.com/coreos/mantle/pluton"

	"github.com/coreos/pkg/capnslog"
)

var plog = capnslog.NewPackageLogger("github.com/coreos/mantle", "pluton/spawn")

type BootkubeManager struct {
	cluster.TestCluster

	firstNode platform.Machine
	info      pluton.Info
	files     scriptFiles
}

func (m *BootkubeManager) AddMasters(n int) ([]platform.Machine, error) {
	return m.provisionNodes(n, true)
}

func (m *BootkubeManager) AddWorkers(n int) ([]platform.Machine, error) {
	return m.provisionNodes(n, false)
}

// MakeSimpleCluster brings up a multi node bootkube cluster with static etcd
// and checks that all nodes are registered before returning. NOTE: If startup
// times become too long there are a few sections of this setup that could be
// run in parallel.
func MakeBootkubeCluster(c cluster.TestCluster, workerNodes int, selfHostEtcd bool) (*pluton.Cluster, error) {
	// options from flags set by main package
	var (
		imageRepo = c.Options["BootkubeRepo"]
		imageTag  = c.Options["BootkubeTag"]
		scriptDir = c.Options["BootkubeScriptDir"]
	)

	// parse in script dir info or use defaults
	var files scriptFiles
	if scriptDir == "" {
		plog.Infof("script dir unspecified, using defaults")
		files.kubeletMaster = defaultKubeletMasterService
		files.kubeletWorker = defaultKubeletWorkerService
	} else {
		var err error
		files, err = parseScriptDir(scriptDir)
		if err != nil {
			return nil, err
		}
	}

	// provision master node running etcd
	masterConfig, err := renderNodeConfig(files.kubeletMaster, true, !selfHostEtcd)
	if err != nil {
		return nil, err
	}
	master, err := c.NewMachine(masterConfig)
	if err != nil {
		return nil, err
	}
	if !selfHostEtcd {
		if err := etcd.GetClusterHealth(master, 1); err != nil {
			return nil, err
		}
	}
	plog.Infof("Master VM (%s) started. It's IP is %s.", master.ID(), master.IP())

	// start bootkube on master
	if err := bootstrapMaster(master, imageRepo, imageTag, selfHostEtcd); err != nil {
		return nil, err
	}

	// parse hyperkube version from service file
	info, err := getVersionFromService(files.kubeletMaster)
	if err != nil {
		return nil, fmt.Errorf("error determining kubernetes version: %v", err)
	}

	// install kubectl on master
	if err := installKubectl(master, info.UpstreamVersion); err != nil {
		return nil, err
	}

	manager := &BootkubeManager{
		TestCluster: c,
		firstNode:   master,
		files:       files,
		info:        info,
	}

	// provision workers
	workers, err := manager.provisionNodes(workerNodes, false)
	if err != nil {
		return nil, err
	}

	cluster := pluton.NewCluster(manager, []platform.Machine{master}, workers, info)

	// check that all nodes appear in kubectl
	if err := cluster.Ready(); err != nil {
		return nil, fmt.Errorf("final node check: %v", err)
	}

	return cluster, nil
}

func renderNodeConfig(kubeletService string, isMaster, startEtcd bool) (string, error) {
	// render template
	tmplData := struct {
		Master         bool
		StartEtcd      bool
		KubeletService string
	}{
		isMaster,
		startEtcd,
		serviceToConfig(kubeletService),
	}

	buf := new(bytes.Buffer)

	tmpl, err := template.New("nodeConfig").Parse(nodeTmpl)
	if err != nil {
		return "", err
	}
	if err := tmpl.Execute(buf, &tmplData); err != nil {
		return "", err
	}

	return buf.String(), nil
}

// The service files we read in from the hack directory need to be indented and
// have a bash variable substituted before being placed in the cloud-config.
func serviceToConfig(s string) string {
	const indent = "        " // 8 spaces to fit in cloud-config

	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = indent + lines[i]
	}

	service := strings.Join(lines, "\n")
	service = strings.Replace(service, "${COREOS_PRIVATE_IPV4}", "$private_ipv4", -1)

	return service
}

func bootstrapMaster(m platform.Machine, imageRepo, imageTag string, selfHostEtcd bool) error {
	const startTimeout = time.Minute * 10 // stop bootkube start if it takes longer then this

	var etcdRenderAdditions, etcdStartAdditions string
	if selfHostEtcd {
		etcdRenderAdditions = "--etcd-servers=http://10.3.0.15:2379  --experimental-self-hosted-etcd"
		etcdStartAdditions = fmt.Sprintf("--etcd-server=http://%s:12379 --experimental-self-hosted-etcd", m.PrivateIP())
	}

	var cmds = []string{
		// disable selinux or rkt run commands fail in odd ways
		"sudo setenforce 0",

		// render assets
		fmt.Sprintf(`sudo /usr/bin/rkt run \
		--volume home,kind=host,source=/home/core \
		--mount volume=home,target=/core \
		--trust-keys-from-https --net=host %s:%s --exec \
		/bootkube -- render --asset-dir=/core/assets --api-servers=https://%s:443,https://%s:443 %s`,
			imageRepo, imageTag, m.IP(), m.PrivateIP(), etcdRenderAdditions),

		// move the local kubeconfig and client cert into expected location
		"sudo chown -R core:core /home/core/assets",
		"sudo mkdir -p /etc/kubernetes",
		"sudo cp /home/core/assets/auth/kubeconfig /etc/kubernetes/",
		// don't fail for backwards compat
		"sudo cp /home/core/assets/tls/ca.crt /etc/kubernetes/ca.crt || true",

		// start kubelet
		"sudo systemctl -q enable --now kubelet",

		// start bootkube (rkt fly makes stderr/stdout seperation work)
		fmt.Sprintf(`sudo /usr/bin/rkt run \
                --stage1-name=coreos.com/rkt/stage1-fly:1.25.0 \
        	--volume home,kind=host,source=/home/core \
        	--mount volume=home,target=/core \
        	--volume manifests,kind=host,source=/etc/kubernetes/manifests \
        	--mount volume=manifests,target=/etc/kubernetes/manifests \
                --trust-keys-from-https \
		%s:%s --exec \
		/bootkube -- start --asset-dir=/core/assets %s`,
			imageRepo, imageTag, etcdStartAdditions),
	}

	// use ssh client to collect stderr and stdout separetly
	// TODO: make the SSH method on a platform.Machine return two slices
	// for stdout/stderr in upstream kola code.
	client, err := m.SSHClient()
	if err != nil {
		return err
	}
	defer client.Close()
	for _, cmd := range cmds {
		session, err := client.NewSession()
		if err != nil {
			return err
		}

		var stdout = bytes.NewBuffer(nil)
		var stderr = bytes.NewBuffer(nil)
		session.Stderr = stderr
		session.Stdout = stdout

		err = session.Start(cmd)
		if err != nil {
			session.Close()
			return err
		}

		// add timeout for each command (mostly used to shorten the bootkube timeout which helps with debugging bootkube start)
		errc := make(chan error)
		go func() { errc <- session.Wait() }()
		select {
		case err := <-errc:
			if err != nil {
				session.Close()
				return fmt.Errorf("SSH session returned error for cmd %s: %s\nSTDOUT:\n%s\nSTDERR:\n%s\n--\n", cmd, err, stdout, stderr)
			}
		case <-time.After(startTimeout):
			session.Close()
			return fmt.Errorf("Timed out waiting %v for cmd %s\nSTDOUT:\n%s\nSTDERR:\n%s\n--\n", startTimeout, cmd, stdout, stderr)
		}
		plog.Infof("Success for cmd %s: %s\nSTDOUT:\n%s\nSTDERR:\n%s\n--\n", cmd, err, stdout, stderr)
		session.Close()
	}

	return nil
}

func (m *BootkubeManager) provisionNodes(n int, master bool) ([]platform.Machine, error) {
	if n == 0 {
		return []platform.Machine{}, nil
	} else if n < 0 {
		return nil, fmt.Errorf("can't provision negative number of nodes")
	}

	var service = m.files.kubeletWorker
	if master {
		service = m.files.kubeletMaster
	}

	config, err := renderNodeConfig(service, master, false)
	if err != nil {
		return nil, err
	}

	configs := make([]string, n)
	for i := range configs {
		configs[i] = config
	}

	nodes, err := platform.NewMachines(m, configs)
	if err != nil {
		return nil, err
	}

	// start kubelet
	for _, node := range nodes {
		// transfer kubeconfig from existing node
		err := platform.TransferFile(m.firstNode, "/etc/kubernetes/kubeconfig", node, "/etc/kubernetes/kubeconfig")
		if err != nil {
			return nil, err
		}

		// transfer client ca cert but soft fail for older verions of bootkube
		err = platform.TransferFile(m.firstNode, "/etc/kubernetes/ca.crt", node, "/etc/kubernetes/ca.crt")
		if err != nil {
			plog.Infof("Warning: unable to transfer client cert to worker: %v", err)
		}

		if err := installKubectl(node, m.info.UpstreamVersion); err != nil {
			return nil, err
		}

		// disable selinux
		_, err = node.SSH("sudo setenforce 0")
		if err != nil {
			return nil, err
		}

		// start kubelet
		_, err = node.SSH("sudo systemctl -q enable --now kubelet.service")
		if err != nil {
			return nil, err
		}
	}
	return nodes, nil

}

type scriptFiles struct {
	kubeletMaster string
	kubeletWorker string
}

func parseScriptDir(scriptDir string) (scriptFiles, error) {
	var files scriptFiles

	b, err := ioutil.ReadFile(filepath.Join(scriptDir, "kubelet.master"))
	if err != nil {
		return scriptFiles{}, fmt.Errorf("failed to read expected kubelet.master file: %v", err)
	}
	files.kubeletMaster = string(b)

	b, err = ioutil.ReadFile(filepath.Join(scriptDir, "kubelet.worker"))
	if err != nil {
		return scriptFiles{}, fmt.Errorf("failed to read expected kubelet.worker file: %v", err)
	}
	files.kubeletWorker = string(b)

	return files, nil
}

func getVersionFromService(kubeletService string) (pluton.Info, error) {
	var versionLine string
	lines := strings.Split(kubeletService, "\n")
	for _, line := range lines {
		if strings.Contains(line, "Environment=KUBELET_IMAGE_TAG=") {
			versionLine = strings.TrimSpace(line)
			break
		}
	}
	if versionLine == "" {
		return pluton.Info{}, fmt.Errorf("could not find kubelet version from service file")
	}

	kubeletTag := strings.TrimPrefix(versionLine, "Environment=KUBELET_IMAGE_TAG=")
	upstream, err := stripSemverSuffix(kubeletTag)
	if err != nil {
		return pluton.Info{}, fmt.Errorf("tag %v: %v", kubeletTag, err)
	}
	semVer := strings.Replace(kubeletTag, "_", "+", 1)
	s := pluton.Info{
		KubeletTag:      kubeletTag,
		Version:         semVer,
		UpstreamVersion: upstream,
	}
	plog.Infof("version detection: %#v", s)

	return s, nil
}

func stripSemverSuffix(v string) (string, error) {
	semverPrefix := regexp.MustCompile(`^v[\d]+\.[\d]+\.[\d]+`)
	v = semverPrefix.FindString(v)
	if v == "" {
		return "", fmt.Errorf("error stripping semver suffix")
	}

	return v, nil
}

func installKubectl(m platform.Machine, upstreamVersion string) error {
	kubeURL := fmt.Sprintf("https://storage.googleapis.com/kubernetes-release/release/%v/bin/linux/amd64/kubectl", upstreamVersion)
	if _, err := m.SSH("wget -q " + kubeURL); err != nil {
		return err
	}
	if _, err := m.SSH("chmod +x ./kubectl"); err != nil {
		return err
	}

	return nil
}
