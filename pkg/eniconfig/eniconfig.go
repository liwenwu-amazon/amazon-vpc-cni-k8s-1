// Copyright 2018 Amazon.com, Inc. or its affiliates. All Rights Reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License"). You may
// not use this file except in compliance with the License. A copy of the
// License is located at
//
//     http://aws.amazon.com/apache2.0/
//
// or in the "license" file accompanying this file. This file is distributed
// on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
// express or implied. See the License for the specific language governing
// permissions and limitations under the License.

// Package eniconfig handles eniconfig CRD
package eniconfig

import (
	"context"
	"os"
	"runtime"
	"sync"
	"time"

	sdk "github.com/operator-framework/operator-sdk/pkg/sdk"
	"github.com/pkg/errors"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/apis/crd/v1alpha1"

	corev1 "k8s.io/api/core/v1"

	k8sutil "github.com/operator-framework/operator-sdk/pkg/util/k8sutil"
	sdkVersion "github.com/operator-framework/operator-sdk/version"

	log "github.com/cihub/seelog"

	"github.com/aws/amazon-vpc-cni-k8s/pkg/eniconfig/portstore"
)

const (
	defaultEniConfigAnnotationDef = "k8s.amazonaws.com/eniConfig"
	defaultEniConfigLabelDef      = "k8s.amazonaws.com/eniConfig"
	eniConfigDefault              = "default"

	// when "ENI_CONFIG_LABEL_DEF is defined, ENIConfigController will use that label key to
	// search if is setting value for eniConfigLabelDef
	// Example:
	//   Node has set label k8s.amazonaws.com/eniConfigCustom=customConfig
	//   We can get that value in controller by setting environmental variable ENI_CONFIG_LABEL_DEF
	//   ENI_CONFIG_LABEL_DEF=k8s.amazonaws.com/eniConfigOverride
	//   This will set eniConfigLabelDef to eniConfigOverride
	envEniConfigAnnotationDef = "ENI_CONFIG_ANNOTATION_DEF"
	envEniConfigLabelDef      = "ENI_CONFIG_LABEL_DEF"

	eniConfigPortStart = 4000
	eniConfigPortNum   = 300
)

var UnKnownNetwork = errors.New("eniconfig: unknown network")

type ENIConfig interface {
	MyENIConfig() (*v1alpha1.ENIConfigSpec, error)
	Getter() *ENIConfigInfo
	AllocatePort(networkName string, podName string) (int, string, string, error)
	ReleasePort(networkName string, port int) error
}

var ErrNoENIConfig = errors.New("eniconfig: eniconfig is not available")

// ENIConfigController defines global context for ENIConfig controller
type ENIConfigController struct {
	eni                    map[string]*v1alpha1.ENIConfigSpec
	portMap                map[string]*portstore.PortMap
	myENI                  string
	eniLock                sync.RWMutex
	myNodeName             string
	eniConfigAnnotationDef string
	eniConfigLabelDef      string
}

// ENIConfigInfo returns locally cached ENIConfigs
type ENIConfigInfo struct {
	ENI                    map[string]v1alpha1.ENIConfigSpec
	MyENI                  string
	EniConfigAnnotationDef string
	EniConfigLabelDef      string
}

// NewENIConfigController creates a new ENIConfig controller
func NewENIConfigController() *ENIConfigController {
	return &ENIConfigController{
		myNodeName:             os.Getenv("MY_NODE_NAME"),
		eni:                    make(map[string]*v1alpha1.ENIConfigSpec),
		portMap:                make(map[string]*portstore.PortMap),
		myENI:                  eniConfigDefault,
		eniConfigAnnotationDef: getEniConfigAnnotationDef(),
		eniConfigLabelDef:      getEniConfigLabelDef(),
	}
}

// NewHandler creates a new handler for sdk
func NewHandler(controller *ENIConfigController) sdk.Handler {
	return &Handler{controller: controller}
}

// Handler stores the ENIConfigController
type Handler struct {
	controller *ENIConfigController
}

// Handle handles ENIconfigs updates from API Server and store them in local cache
func (h *Handler) Handle(ctx context.Context, event sdk.Event) error {
	switch o := event.Object.(type) {
	case *v1alpha1.ENIConfig:

		eniConfigName := o.GetName()

		curENIConfig := o.DeepCopy()

		if event.Deleted {
			log.Infof("Deleting ENIConfig: %s", eniConfigName)
			h.controller.eniLock.Lock()
			defer h.controller.eniLock.Unlock()
			delete(h.controller.eni, eniConfigName)
			delete(h.controller.portMap, eniConfigName)
			return nil
		}

		log.Infof("Handle ENIConfig Add/Update:  %s, %v, %v, %s", eniConfigName, curENIConfig, curENIConfig.Spec.SecurityGroups, curENIConfig.Spec.Subnet)
		log.Infof("eniConfigName: %s, CGWIP: %s, VNID: %s", eniConfigName, curENIConfig.Spec.CGWIP, curENIConfig.Spec.VNID)

		h.controller.eniLock.Lock()
		defer h.controller.eniLock.Unlock()

		_, ok := h.controller.eni[eniConfigName]
		h.controller.eni[eniConfigName] = &curENIConfig.Spec

		if !ok {
			h.controller.portMap[eniConfigName] = portstore.PortMapInit(eniConfigPortNum, eniConfigPortStart)
		}

	case *corev1.Node:

		log.Infof("Handle corev1.Node: %s, %v, %v", o.GetName(), o.GetAnnotations(), o.GetLabels())
		// Get annotations if not found get labels if not found fallback use default
		if h.controller.myNodeName == o.GetName() {

			val, ok := o.GetAnnotations()[h.controller.eniConfigAnnotationDef]
			if !ok {
				val, ok = o.GetLabels()[h.controller.eniConfigLabelDef]
				if !ok {
					val = eniConfigDefault
				}
			}

			h.controller.eniLock.Lock()
			defer h.controller.eniLock.Unlock()
			h.controller.myENI = val
			log.Infof(" Setting myENI to: %s", val)
		}
	}
	return nil
}

func printVersion() {
	log.Infof("Go Version: %s", runtime.Version())
	log.Infof("Go OS/Arch: %s/%s", runtime.GOOS, runtime.GOARCH)
	log.Infof("operator-sdk Version: %v", sdkVersion.Version)
}

// Start kicks off ENIConfig controller
func (eniCfg *ENIConfigController) Start() {
	printVersion()

	sdk.ExposeMetricsPort()

	resource := "crd.k8s.amazonaws.com/v1alpha1"
	kind := "ENIConfig"
	namespace, err := k8sutil.GetWatchNamespace()
	if err != nil {
		log.Errorf("failed to get watch namespace: %v", err)
	}
	resyncPeriod := time.Second * 5
	log.Infof("Watching %s, %s, %s, %d", resource, kind, namespace, resyncPeriod)
	sdk.Watch(resource, kind, namespace, resyncPeriod)
	sdk.Watch("/v1", "Node", corev1.NamespaceAll, resyncPeriod)
	sdk.Handle(NewHandler(eniCfg))
	sdk.Run(context.TODO())
}

func (eniCfg *ENIConfigController) AllocatePort(networkName string, podName string) (int, string, string, error) {
	log.Debugf("AllocatePort: networkName: %s, podName %s", networkName, podName)
	eni, ok := eniCfg.eni[networkName]
	if !ok {
		log.Errorf("AllocatePort: failed to find eni config for network :%v pod %v", networkName, podName)
		return -1, "", "", UnKnownNetwork
	}

	portMap, ok := eniCfg.portMap[networkName]

	if !ok {
		log.Errorf("AllocatePort: failed to find portMap for network :%v pod %v", networkName, podName)
		return -1, "", "", UnKnownNetwork
	}

	port, err := portstore.PortMapAllocPort(portMap, podName)

	if err != nil {
		log.Errorf("AllocatePort: failed to allocate port: err=%v for pod :%v", err, podName)
		return -1, "", "", err
	}

	log.Debugf("AllocatePort: Successfully allocated port :%v , CGWIP: %v, VNID: %v for pod %v", port, eni.CGWIP, eni.VNID, podName)
	return port, eni.CGWIP, eni.VNID, nil

}

func (eniCfg *ENIConfigController) ReleasePort(networkName string, port int) error {

	log.Debugf("ReleasePort: networkName %v, port %v", networkName, port)
	portMap, ok := eniCfg.portMap[networkName]

	if !ok {
		log.Errorf("ReleasePort: failed to find portMap for network :%v pod %v", networkName, port)
		return UnKnownNetwork
	}

	err := portstore.PortMapReleasePort(portMap, port)

	if err != nil {
		log.Errorf("ReleasePort, failed on PortMapRelease for network %v, port %v", networkName, port)
		return err
	}
	return nil
}

func (eniCfg *ENIConfigController) Getter() *ENIConfigInfo {
	output := &ENIConfigInfo{
		ENI: make(map[string]v1alpha1.ENIConfigSpec),
	}
	eniCfg.eniLock.Lock()
	defer eniCfg.eniLock.Unlock()

	output.MyENI = eniCfg.myENI
	output.EniConfigAnnotationDef = getEniConfigAnnotationDef()
	output.EniConfigLabelDef = getEniConfigLabelDef()

	for name, val := range eniCfg.eni {
		output.ENI[name] = *val
	}

	return output
}

// MyENIConfig returns the security
func (eniCfg *ENIConfigController) MyENIConfig() (*v1alpha1.ENIConfigSpec, error) {
	eniCfg.eniLock.Lock()
	defer eniCfg.eniLock.Unlock()

	myENIConfig, ok := eniCfg.eni[eniCfg.myENI]

	if ok {
		return &v1alpha1.ENIConfigSpec{
			SecurityGroups: myENIConfig.SecurityGroups,
			Subnet:         myENIConfig.Subnet,
		}, nil
	}
	return nil, ErrNoENIConfig
}

// getEniConfigAnnotationDef returns eniConfigAnnotation
func getEniConfigAnnotationDef() string {
	inputStr, found := os.LookupEnv(envEniConfigAnnotationDef)

	if !found {
		return defaultEniConfigAnnotationDef
	}
	if len(inputStr) > 0 {
		log.Debugf("Using ENI_CONFIG_ANNOTATION_DEF %v", inputStr)
		return inputStr
	}

	return defaultEniConfigAnnotationDef
}

// getEniConfigLabelDef returns eniConfigLabel name
func getEniConfigLabelDef() string {
	inputStr, found := os.LookupEnv(envEniConfigLabelDef)

	if !found {
		return defaultEniConfigLabelDef
	}
	if len(inputStr) > 0 {
		log.Debugf("Using ENI_CONFIG_LABEL_DEF %v", inputStr)
		return inputStr
	}

	return defaultEniConfigLabelDef
}
