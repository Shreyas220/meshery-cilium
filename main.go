// Copyright 2022 Layer5 Authors
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
package main

import (
	"fmt"
	"os"
	"path"
	"strings"
	"time"

	"github.com/layer5io/meshery-adapter-library/adapter"
	"github.com/layer5io/meshery-adapter-library/api/grpc"
	"github.com/layer5io/meshery-cilium/cilium"
	"github.com/layer5io/meshery-cilium/cilium/oam"
	"github.com/layer5io/meshery-cilium/internal/config"
	configprovider "github.com/layer5io/meshkit/config/provider"
	"github.com/layer5io/meshkit/logger"
	"github.com/layer5io/meshkit/utils/manifests"
	smp "github.com/layer5io/service-mesh-performance/spec"
)

var (
	serviceName = "cilium-adapter"
	version     = "edge"
	gitsha      = "none"
)

func init() {
	// Create the config path if it doesn't exists as the entire adapter
	// expects that directory to exists, which may or may not be true
	if err := os.MkdirAll(path.Join(config.RootPath(), "bin"), 0750); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// main is the entrypoint of the adaptor
func main() {
	// Initialize Logger instance
	log, err := logger.New(serviceName, logger.Options{
		Format:     logger.SyslogLogFormat,
		DebugLevel: isDebug(),
	})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	err = os.Setenv("KUBECONFIG", path.Join(
		config.KubeConfigDefaults[configprovider.FilePath],
		fmt.Sprintf("%s.%s", config.KubeConfigDefaults[configprovider.FileName], config.KubeConfigDefaults[configprovider.FileType])),
	)

	if err != nil {
		// Fail silently
		log.Warn(err)
	}

	// Initialize application specific configs and dependencies
	// App and request config
	cfg, err := config.New(configprovider.ViperKey)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	service := &grpc.Service{}
	err = cfg.GetObject(adapter.ServerKey, service)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	kubeconfigHandler, err := config.NewKubeconfigBuilder(configprovider.ViperKey)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	// // Initialize Tracing instance
	// tracer, err := tracing.New(service.Name, service.TraceURL)
	// if err != nil {
	//      log.Err("Tracing Init Failed", err.Error())
	//      os.Exit(1)
	// }

	// Initialize Handler intance
	handler := cilium.New(cfg, log, kubeconfigHandler)
	handler = adapter.AddLogger(log, handler)

	service.Handler = handler
	service.Channel = make(chan interface{}, 10)
	service.StartedAt = time.Now()
	service.Version = version
	service.GitSHA = gitsha
	go registerCapabilities(service.Port, log)        //Registering static capabilities
	go registerDynamicCapabilities(service.Port, log) //Registering latest capabilities periodically

	// Server Initialization
	log.Info("Adaptor Listening at port: ", service.Port)
	err = grpc.Start(service, nil)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}
}

func isDebug() bool {
	return os.Getenv("DEBUG") == "true"
}

func mesheryServerAddress() string {
	meshReg := os.Getenv("MESHERY_SERVER")

	if meshReg != "" {
		if strings.HasPrefix(meshReg, "http") {
			return meshReg
		}

		return "http://" + meshReg
	}

	return "http://localhost:9081"
}

func serviceAddress() string {
	svcAddr := os.Getenv("SERVICE_ADDR")

	if svcAddr != "" {
		return svcAddr
	}

	return "localhost"
}

func registerCapabilities(port string, log logger.Handler) {
	// Register workloads
	log.Info("Registering static workloads...")
	if err := oam.RegisterWorkloads(mesheryServerAddress(), serviceAddress()+":"+port); err != nil {
		log.Info(err.Error())
	}
	log.Info("Registering static workloads completed")
	// Register traits
	if err := oam.RegisterTraits(mesheryServerAddress(), serviceAddress()+":"+port); err != nil {
		log.Info(err.Error())
	}
}

func registerDynamicCapabilities(port string, log logger.Handler) {
	registerWorkloads(port, log)
	//Start the ticker
	const reRegisterAfter = 24
	ticker := time.NewTicker(reRegisterAfter * time.Hour)
	for {
		<-ticker.C
		registerWorkloads(port, log)
	}

}
func registerWorkloads(port string, log logger.Handler) {
	var url string
	var gm string

	//If a URL is passed from env variable, it will be used for component generation with default method being "using manifests"
	// In case a helm chart URL is passed, COMP_GEN_METHOD env variable should be set to Helm otherwise the component generation fails
	if os.Getenv("COMP_GEN_URL") != "" {
		url = os.Getenv("COMP_GEN_URL")
		if os.Getenv("COMP_GEN_METHOD") == "Helm" || os.Getenv("COMP_GEN_METHOD") == "Manifest" {
			gm = os.Getenv("COMP_GEN_METHOD")
		} else {
			gm = adapter.Manifests
		}
		log.Info("Registering workload components from url ", url, " using ", gm, " method...")
	} else {
		log.Info("Registering latest workload components for version ", version)
		//default way
		url = "https://raw.githubusercontent.com/cilium/cilium/" + version + "/install/kubernetes/cilium/Chart.yaml"
		gm = adapter.Manifests
	}
	// Register workloads
	if err := adapter.RegisterWorkLoadsDynamically(mesheryServerAddress(), serviceAddress()+":"+port, &adapter.DynamicComponentsConfig{
		TimeoutInMinutes: 30,
		URL:              url,
		GenerationMethod: gm,
		Config: manifests.Config{
			Name:        smp.ServiceMesh_Type_name[int32(smp.ServiceMesh_CILIUM_SERVICE_MESH)],
			MeshVersion: version,
			Filter: manifests.CrdFilter{
				RootFilter:    []string{"$[?(@.kind==\"CustomResourceDefinition\")]"},
				NameFilter:    []string{"$..[\"spec\"][\"names\"][\"kind\"]"},
				VersionFilter: []string{"$[0]..spec.versions[0]"},
				GroupFilter:   []string{"$[0]..spec"},
				SpecFilter:    []string{"$[0]..openAPIV3Schema.properties.spec"},
				ItrFilter:     []string{"$[?(@.spec.names.kind"},
				ItrSpecFilter: []string{"$[?(@.spec.names.kind"},
				VField:        "name",
				GField:        "group",
			},
		},
		Operation: config.CiliumOperation,
	}); err != nil {
		log.Info(err.Error())
		return
	}
	log.Info("Latest workload components successfully registered.")
}
