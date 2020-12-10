/*
Copyright 2020 SUSE

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package helm

import (
	"bytes"
	"crypto/sha256"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"os"
	"path"
	"sync"
	"time"

	"helm.sh/helm/v3/pkg/action"
	"helm.sh/helm/v3/pkg/chart/loader"
	"helm.sh/helm/v3/pkg/kube"
	"helm.sh/helm/v3/pkg/release"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/yaml"
)

const (
	driver           = "secret"
	defaultNamespace = "default"

	// Empty values for these mean the internal defaults will be used.
	defaultKubeConfig = ""
	defaultContext    = ""

	// As old versions of Kubernetes had a limit on names of 63 characters, Helm uses 53, reserving
	// 10 characters for charts to add data.
	helmMaxNameLength = 53
)

// Installer is the interface that wraps the Install method for Helm.
type Installer interface {
	Install(name string, chartInfo ChartInfo, opts InstallOpts) error
}

// ChartInfo contains information necessary to identify a helm chart archive for installation.
type ChartInfo struct {
	URL, SHA256Sum string
}

// NamespaceOpt represents a namespace string that implements the ValueOrDefault
// method. It's used to return the default namespace in the case of the value
// being empty.
type NamespaceOpt string

// ValueOrDefault returns the default namespace if the NamespaceOpt value is not set.
func (ns *NamespaceOpt) ValueOrDefault() string {
	if *ns == "" {
		return defaultNamespace
	}
	return string(*ns)
}

// InstallOpts are the Helm install options.
type InstallOpts struct {
	Atomic      bool
	Description string
	Namespace   NamespaceOpt
	Timeout     time.Duration
	Values      map[string]interface{}
	Wait        bool
}

// Uninstaller is the interface that wraps the Uninstall method for Helm.
type Uninstaller interface {
	Uninstall(name string, opts UninstallOpts) error
}

// UninstallOpts are the Helm uninstall options.
type UninstallOpts struct {
	Description string
	Namespace   NamespaceOpt
}

// Getter is the interface that wraps the Get method for Helm.
type Getter interface {
	Get(name string, opts GetOpts) error
}

// GetOpts are the Helm get options.
type GetOpts struct {
	Namespace NamespaceOpt
}

// Client is a Helm client that satisfies the Installer and Uninstaller
// interfaces.
type Client struct {
	chartCache *ChartCache
}

// NewClient constructs a new Client.
func NewClient(chartCache *ChartCache) *Client {
	return &Client{
		chartCache: chartCache,
	}
}

// Install satisfies the Installer interface. It installs a Helm chart from a
// ChartInfo using its URL and SHA 256 sum to verify its integrity. The chart
// tarball is cached for future uses based on the checksum.
func (c *Client) Install(name string, chartInfo ChartInfo, opts InstallOpts) error {
	if len(name) > helmMaxNameLength {
		err := fmt.Errorf(
			"invalid release name %q: names cannot exceed %d characters",
			name,
			helmMaxNameLength)
		return fmt.Errorf("failed to install Helm chart %q: %w", name, err)
	}

	chartFile, err := c.chartCache.Fetch(chartInfo)
	if err != nil {
		return fmt.Errorf("failed to install Helm chart %q: %w", name, err)
	}
	defer chartFile.Close()

	chart, err := loader.LoadArchive(chartFile)
	if err != nil {
		return fmt.Errorf("failed to install Helm chart %q: %w", name, err)
	}

	namespace := opts.Namespace.ValueOrDefault()
	cfg, err := c.config(namespace)
	if err != nil {
		return fmt.Errorf("failed to install Helm chart %q: %w", name, err)
	}
	client := action.NewInstall(cfg)
	client.ReleaseName = name
	client.Namespace = namespace
	client.Atomic = opts.Atomic
	client.Description = opts.Description
	client.Timeout = opts.Timeout
	client.Wait = opts.Wait

	if _, err := client.Run(chart, opts.Values); err != nil {
		return fmt.Errorf("failed to install Helm chart %q: %w", name, err)
	}

	return nil
}

// Uninstall satisfies the Uninstaller interface. It uninstalls a Helm
// installation using its name.
func (c *Client) Uninstall(name string, opts UninstallOpts) error {
	// TODO: implement.
	return nil
}

// Get satisfies the Get interface. It gets a Helm installation using its name.
func (c *Client) Get(name string, opts GetOpts) (*Release, error) {
	namespace := opts.Namespace.ValueOrDefault()
	cfg, err := c.config(namespace)
	if err != nil {
		return nil, fmt.Errorf("failed to get Helm release %q: %w", name, err)
	}
	client := action.NewGet(cfg)
	res, err := client.Run(name)
	if err != nil {
		return nil, fmt.Errorf("failed to get Helm release %q: %w", name, err)
	}
	return &Release{res}, nil
}

func (c *Client) config(namespace string) (*action.Configuration, error) {
	restGetter := kube.GetConfig(defaultKubeConfig, defaultContext, namespace)
	debug := func(format string, v ...interface{}) {
		// TODO(f0rmiga): provide a logic for Helm debugging logs.
	}
	cfg := &action.Configuration{}
	if err := cfg.Init(restGetter, namespace, driver, debug); err != nil {
		return nil, fmt.Errorf("failed to provide action configuration: %w", err)
	}
	return cfg, nil
}

// ChartCache manages the local chart cache.
type ChartCache struct {
	cachePath string

	osStat     func(name string) (os.FileInfo, error)
	osOpenFile func(name string, flag int, perm os.FileMode) (*os.File, error)
	httpGetter HTTPGetter

	mutex sync.Mutex
}

// NewChartCache constructs a new ChartCache.
func NewChartCache(cachePath string) *ChartCache {
	return &ChartCache{
		cachePath:  cachePath,
		osStat:     os.Stat,
		osOpenFile: os.OpenFile,
		httpGetter: http.DefaultClient,
	}
}

// Fetch downloads a chart tarball if needed, adding it to the cache. The
// returned tarball is always a handle to the cached file.
func (cc *ChartCache) Fetch(chartInfo ChartInfo) (io.ReadCloser, error) {
	fileName := fmt.Sprintf("%s.tgz", chartInfo.SHA256Sum)
	filePath := path.Join(cc.cachePath, fileName)
	if _, err := cc.osStat(filePath); err == nil {
		r, err := cc.osOpenFile(filePath, os.O_RDONLY, 0)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch chart from %q: %w", chartInfo.URL, err)
		}
		return r, nil
	} else if !os.IsNotExist(err) {
		// The error exists and it's not of the type non-existent file. I.e. an
		// unexpected error.
		return nil, fmt.Errorf("failed to fetch chart from %q: %w", chartInfo.URL, err)
	}

	res, err := cc.httpGetter.Get(chartInfo.URL)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chart from %q: %w", chartInfo.URL, err)
	}
	defer res.Body.Close()
	if err := cc.cache(filePath, res.Body, chartInfo.SHA256Sum); err != nil {
		return nil, fmt.Errorf("failed to fetch chart from %q: %w", chartInfo.URL, err)
	}

	r, err := cc.osOpenFile(filePath, os.O_RDONLY, 0)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch chart from %q: %w", chartInfo.URL, err)
	}
	return r, nil
}

func (cc *ChartCache) cache(filePath string, data io.Reader, sha256sum string) error {
	cc.mutex.Lock()
	defer cc.mutex.Unlock()

	w, err := cc.osOpenFile(filePath, os.O_CREATE|os.O_WRONLY, 0)
	if err != nil {
		return fmt.Errorf("failed to cache %q: %w", filePath, err)
	}
	defer w.Close()

	h := sha256.New()

	if _, err := fanout(data, w, h); err != nil {
		w.Close()
		os.Remove(filePath)
		return fmt.Errorf("failed to cache %q: %w", filePath, err)
	}

	calculatedHashSum := fmt.Sprintf("%x", h.Sum(nil))
	if sha256sum != calculatedHashSum {
		w.Close()
		os.Remove(filePath)
		err := fmt.Errorf(
			"provided checksum %q does not match calculated %q",
			sha256sum,
			calculatedHashSum)
		return fmt.Errorf("failed to cache %q: %w", filePath, err)
	}

	return nil
}

// HTTPGetter wraps the HTTP Get method.
type HTTPGetter interface {
	Get(url string) (resp *http.Response, err error)
}

// fanout writes the data from the reader to the many writers it can take.
func fanout(r io.Reader, ws ...io.Writer) (written int64, err error) {
	var ir io.Reader = r
	for _, w := range ws {
		ir = io.TeeReader(ir, w)
	}
	return io.Copy(ioutil.Discard, ir)
}

// Release is a wrapper around "helm.sh/helm/v3/pkg/release".Release for
// extending its functionality.
type Release struct {
	*release.Release
}

// ListKubernetesObjects lists the Kubernetes objects installed by the Helm
// release.
func (rel *Release) ListKubernetesObjects() ([]KubernetesObject, error) {
	objs := []KubernetesObject{}
	decoder := yaml.NewYAMLToJSONDecoder(bytes.NewBufferString(rel.Manifest))
	for {
		obj := KubernetesObject{}
		if err := decoder.Decode(&obj); err != nil {
			if err == io.EOF {
				break
			}
			return nil, fmt.Errorf("failed to list resources: %w", err)
		}
		objs = append(objs, obj)
	}
	return objs, nil
}

// KubernetesObject contains the type and metadata of a Kubernetes object. It's
// useful for unmarshalling objects regardless of the payloads.
type KubernetesObject struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
}
