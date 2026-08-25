/*
 * This file is part of the KubeVirt project
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 * Copyright The KubeVirt Authors.
 *
 */

package main

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	k8sv1 "k8s.io/api/core/v1"
	k8coresv1 "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"
	"k8s.io/client-go/util/flowcontrol"

	"kubevirt.io/client-go/kubecli"
	"kubevirt.io/client-go/kubevirt/scheme"
	"kubevirt.io/client-go/log"
	clientutil "kubevirt.io/client-go/util"

	"kubevirt.io/kubevirt/pkg/controller"
	managedclaim "kubevirt.io/kubevirt/pkg/managed-claim"
	"kubevirt.io/kubevirt/pkg/managed-claim/aligner"
	"kubevirt.io/kubevirt/pkg/service"
	"kubevirt.io/kubevirt/pkg/util/ratelimiter"
	virtconfig "kubevirt.io/kubevirt/pkg/virt-config"
	"kubevirt.io/kubevirt/pkg/virt-controller/leaderelectionconfig"
)

const (
	defaultHost = "0.0.0.0"
	defaultPort = 8080

	defaultGracefulShutdownSeconds = 30
	maxRetryCount                  = 10
	leaseName                      = "virt-managed-claim-controller"
	componentName                  = "virt-managed-claim-controller"
	threadiness                    = 10
)

type managedClaimControllerApp struct {
	service.ServiceListen

	virtCli        kubecli.KubevirtClient
	namespace      string
	LeaderElection leaderelectionconfig.Configuration

	reloadableRateLimiter *ratelimiter.ReloadableRateLimiter

	ctx context.Context
}

func (app *managedClaimControllerApp) Run() {
	var err error

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	app.ctx = ctx

	app.LeaderElection = leaderelectionconfig.DefaultLeaderElectionConfiguration()
	app.reloadableRateLimiter = ratelimiter.NewReloadableRateLimiter(
		flowcontrol.NewTokenBucketRateLimiter(virtconfig.DefaultVirtControllerQPS, virtconfig.DefaultVirtControllerBurst))

	clientConfig := app.mustGetClientConfig()
	clientConfig.RateLimiter = app.reloadableRateLimiter
	app.mustGetClient(clientConfig)

	app.namespace, err = clientutil.GetNamespace()
	if err != nil {
		log.Log.Criticalf("Error searching for namespace: %v", err)
		os.Exit(2)
	}
	log.Log.V(1).Infof("running in namespace %s", app.namespace)

	factory := controller.NewKubeInformerFactory(app.virtCli.RestClient(), app.virtCli, app.virtCli, nil, app.namespace)
	vmiInformer := factory.VMI()
	provisionerInformer := factory.ManagedClaimProvisioner()
	claimInformer := factory.ResourceClaim()

	stop := app.ctx.Done()

	sigint := make(chan os.Signal, 1)
	signal.Notify(sigint, os.Interrupt, os.Kill, syscall.SIGHUP, syscall.SIGTERM, syscall.SIGQUIT)
	go func() {
		s := <-sigint
		log.Log.Infof("received signal %s, initiating graceful shutdown", s.String())
		cancel()
	}()

	reconciler := managedclaim.NewReconciler(
		aligner.ProvisionerName,
		&aligner.Provisioner{},
		app.virtCli,
		managedclaim.NewInformerProvisionerStore(provisionerInformer),
	)

	recorder := app.getNewRecorder(k8sv1.NamespaceAll, componentName)

	managedClaimController, err := managedclaim.NewController(
		reconciler,
		recorder,
		vmiInformer,
		provisionerInformer,
		claimInformer,
	)
	if err != nil {
		panic(err)
	}

	factory.Start(stop)
	app.runWithLeaderElection(managedClaimController, stop)
}

func (app *managedClaimControllerApp) mustGetClientConfig() *rest.Config {
	var clientConfig *rest.Config
	var err error
	for retryCount := 0; retryCount < maxRetryCount; retryCount++ {
		clientConfig, err = kubecli.GetKubevirtClientConfig()
		if err == nil {
			return clientConfig
		}
		log.Log.Errorf("unable to get kubevirt client config %v", err)
		time.Sleep(time.Duration(2^(retryCount+1)) * time.Millisecond)
	}
	panic(fmt.Errorf("unable to get kubevirt client config after %d retries %v", maxRetryCount, err))
}

func (app *managedClaimControllerApp) mustGetClient(clientConfig *rest.Config) {
	var err error
	for retryCount := 0; retryCount < maxRetryCount; retryCount++ {
		app.virtCli, err = kubecli.GetKubevirtClientFromRESTConfig(clientConfig)
		if err == nil {
			return
		}
		log.Log.Errorf("unable to get kubevirt client from rest config %v", err)
		time.Sleep(time.Duration(2^(retryCount+1)) * time.Millisecond)
	}
	panic(fmt.Errorf("unable to get kubevirt client from rest config after %d retries %v", maxRetryCount, err))
}

func (app *managedClaimControllerApp) runWithLeaderElection(managedClaimController *managedclaim.Controller, stop <-chan struct{}) {
	recorder := app.getNewRecorder(k8sv1.NamespaceAll, leaseName)

	id, err := os.Hostname()
	if err != nil {
		log.Log.Criticalf("unable to get hostname: %v", err)
		panic(err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", app.healthzHandler)
	server := &http.Server{
		Addr:    app.Address(),
		Handler: mux,
	}

	go func() {
		log.Log.V(2).Infof("/healthz listening on %s", server.Addr)
		if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Log.Errorf("healthz server error: %v", err)
		}
	}()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-stop
		httpShutdownCtx, httpShutdownCancel := context.WithTimeout(context.Background(), defaultGracefulShutdownSeconds*time.Second)
		defer httpShutdownCancel()
		if err := server.Shutdown(httpShutdownCtx); err != nil {
			log.Log.Errorf("server shutdown error: %v", err)
		}
	}()

	rl, err := resourcelock.New(app.LeaderElection.ResourceLock,
		app.namespace,
		leaseName,
		app.virtCli.CoreV1(),
		app.virtCli.CoordinationV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		})
	if err != nil {
		panic(err)
	}

	controllerContext, controllerCancel := context.WithCancel(context.Background())

	wg.Add(1)
	leaderElector, err := leaderelection.NewLeaderElector(
		leaderelection.LeaderElectionConfig{
			Lock:          rl,
			LeaseDuration: app.LeaderElection.LeaseDuration.Duration,
			RenewDeadline: app.LeaderElection.RenewDeadline.Duration,
			RetryPeriod:   app.LeaderElection.RetryPeriod.Duration,
			Callbacks: leaderelection.LeaderCallbacks{
				OnStartedLeading: func(_ context.Context) {
					managedClaimController.Run(threadiness, controllerContext.Done())
					log.Log.Info("successfully shut down controller")
					wg.Done()
				},
				OnStoppedLeading: func() {
					log.Log.Error("leaderelection lost, shutting down controller")
					controllerCancel()
				},
			},
		})
	if err != nil {
		panic(err)
	}
	leaderElector.Run(app.ctx)
	wg.Wait()
}

func (app *managedClaimControllerApp) getNewRecorder(namespace string, name string) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartRecordingToSink(&k8coresv1.EventSinkImpl{Interface: app.virtCli.CoreV1().Events(namespace)})
	return eventBroadcaster.NewRecorder(scheme.Scheme, k8sv1.EventSource{Component: name})
}

func (app *managedClaimControllerApp) healthzHandler(w http.ResponseWriter, _ *http.Request) {
	io.WriteString(w, "OK")
}

func (app *managedClaimControllerApp) AddFlags() {
	app.InitFlags()
	app.AddCommonFlags()

	if app.BindAddress == "" {
		app.BindAddress = defaultHost
	}
	if app.Port == 0 {
		app.Port = defaultPort
	}
}

func main() {
	app := &managedClaimControllerApp{}
	service.Setup(app)
	app.Run()
	log.Log.Info("successfully shutdown")
}
