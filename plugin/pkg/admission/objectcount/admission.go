/*
Copyright 2024 The Kubernetes Authors.

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

package objectcount

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/prometheus/common/expfmt"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apiserver/pkg/admission"
	admissioninitailizer "k8s.io/apiserver/pkg/admission/initializer"
	"k8s.io/client-go/kubernetes"
	"k8s.io/klog/v2"
	objectcountapi "k8s.io/kubernetes/plugin/pkg/admission/objectcount/apis/objectcount"
)

const (
	// PluginName indicates name of admission plugin.
	PluginName = "ObjectCount"

	metricsPath  = "/metrics"
	targetMetric = "apiserver_storage_objects"
)

// Register registers a plugin
func Register(plugins *admission.Plugins) {
	plugins.Register(PluginName,
		func(config io.Reader) (admission.Interface, error) {
			// load the configuration provided (if any)
			configuration, err := LoadConfiguration(config)
			if err != nil {
				return nil, err
			}
			return NewObjectCount(configuration)
		})
}

// ObjectCount enforces usage limits on a per-resource basis in the namespace
type ObjectCount struct {
	*admission.Handler
	client kubernetes.Interface

	timeout      time.Duration
	defaultLimit *int64
	limits       map[string]int64

	// admitResource map ifself should be considered as immutable and
	// to update the map you should update the reference to point a new map.
	admitResource map[string]bool
	mutex         sync.RWMutex
}

var _ admission.ValidationInterface = &ObjectCount{}

var _ admissioninitailizer.WantsExternalKubeClientSet = &ObjectCount{}

func NewObjectCount(config *objectcountapi.Configuration) (*ObjectCount, error) {
	// return a noop admission controller when there's no meaningful config.
	if len(config.Limits) == 0 && config.DefaultLimit == nil {
		return &ObjectCount{
			Handler: admission.NewHandler(),
		}, nil
	}

	oc := &ObjectCount{
		Handler:       admission.NewHandler(admission.Create),
		timeout:       time.Duration(*config.RefreshIntervalSeconds * int64(time.Second)),
		defaultLimit:  config.DefaultLimit,
		limits:        config.Limits,
		admitResource: map[string]bool{},
		mutex:         sync.RWMutex{},
	}

	go oc.updateObjectCount()
	return oc, nil
}

// SetExternalKubeClientSet registers the client into ObjectCount
func (oc *ObjectCount) SetExternalKubeClientSet(client kubernetes.Interface) {
	oc.client = client
}

func (oc *ObjectCount) updateObjectCount() {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	// TODO: send singal to done channel when shutting down
	done := make(chan bool)
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), oc.timeout)
			defer cancel()

			rawResp, err := oc.client.CoreV1().RESTClient().Get().RequestURI(metricsPath).DoRaw(ctx)
			if err != nil {
				klog.V(2).Infof("objectCount admission controller is unable to get apiserver metrics: %v", err)
				continue
			}

			parser := &expfmt.TextParser{}
			families, err := parser.TextToMetricFamilies(bytes.NewReader(rawResp))
			if err != nil {
				klog.V(2).Infof("objectCount admission controller is unable to parse apiserver metrics: %v", err)
				continue
			}
			family, found := families[targetMetric]
			if !found {
				klog.V(2).Infof("objectCount admission controller is unable to find the target metric: %v", targetMetric)
				continue
			}
			newShouldAdmit := map[string]bool{}
			for _, metric := range family.Metric {
				var resourceName string
				if len(metric.Label) == 1 {
					resourceName = metric.Label[0].GetValue()
				} else {
					for _, label := range metric.Label {
						if label.GetName() == "resource" {
							resourceName = label.GetValue()
							break
						}
					}
					if resourceName == "" {
						klog.V(2).Infof("objectCount admission controller is unable to find the expected resouce label: %v", targetMetric)
						continue
					}
				}
				newShouldAdmit[resourceName] = oc.shouldAdmit(resourceName, int64(metric.Gauge.GetValue()))
			}

			func() {
				oc.mutex.Lock()
				defer oc.mutex.Unlock()
				oc.admitResource = newShouldAdmit
			}()
		}
	}
}

// ValidateInitialization verifies the ObjectCount object has been properly initialized
func (oc *ObjectCount) ValidateInitialization() error {
	if oc.client == nil {
		return fmt.Errorf("objectCount admission controller missing client")
	}
	return nil
}

// Validate admits resources into cluster that do not violate any defined LimitRange in the namespace
func (oc *ObjectCount) Validate(ctx context.Context, a admission.Attributes, o admission.ObjectInterfaces) (err error) {
	// Skip request to subresources
	if a.GetSubresource() != "" {
		return nil
	}
	res := a.GetResource()
	resStr := res.GroupResource().String()

	var admitResource map[string]bool
	func() {
		oc.mutex.RLock()
		defer oc.mutex.RUnlock()
		// The admitResource map is immutable, so we only need to get a reference of the map while holding the lock
		admitResource = oc.admitResource
	}()
	if admit, found := admitResource[resStr]; found && !admit {
		return apierrors.NewTooManyRequestsError(fmt.Sprintf("object count for %v exceeds the limit, please clean up your resources and retry later", resStr))
	}
	return nil
}

func (oc *ObjectCount) shouldAdmit(resource string, cnt int64) bool {
	threshold, foundThreshold := oc.limits[resource]
	if !foundThreshold {
		if oc.defaultLimit == nil {
			return true
		} else {
			threshold = *oc.defaultLimit
		}
	}
	return cnt <= threshold
}
