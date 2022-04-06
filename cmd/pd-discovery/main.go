// Copyright 2019 TiKV Project Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"flag"
	"net/http"
	_ "net/http/pprof"
	"time"

	"github.com/tikv/tikv-operator/pkg/client/clientset/versioned"
	"github.com/tikv/tikv-operator/pkg/discovery/server"
	"github.com/tikv/tikv-operator/pkg/verflag"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/component-base/logs"
	"k8s.io/component-base/version"
	"k8s.io/klog"
)

var (
	printVersion bool
	port         int
)

func init() {
	flag.BoolVar(&printVersion, "version", false, "Show version and quit")
	flag.IntVar(&port, "port", 10261, "The port that the tidb discovery's http service runs on (default 10261)")
	flag.Parse()
}

func main() {
	verflag.PrintAndExitIfRequested()
	klog.Infof("PD Discovery: %s", version.Get())

	logs.InitLogs()
	defer logs.FlushLogs()

	flag.CommandLine.VisitAll(func(flag *flag.Flag) {
		klog.V(1).Infof("FLAG: --%s=%q", flag.Name, flag.Value)
	})

	cfg, err := rest.InClusterConfig()
	if err != nil {
		klog.Fatalf("failed to get config: %v", err)
	}
	cli, err := versioned.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("failed to create Clientset: %v", err)
	}
	kubeCli, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		klog.Fatalf("failed to get kubernetes Clientset: %v", err)
	}

	go wait.Forever(func() {
		server.StartServer(cli, kubeCli, port)
	}, 5*time.Second)
	klog.Fatal(http.ListenAndServe(":6060", nil))
}
