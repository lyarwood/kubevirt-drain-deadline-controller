package main

import (
	"flag"
	"os"
	"time"

	kubevirtv1 "kubevirt.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	"github.com/lyarwood/kubevirt-drain-deadline-controller/pkg/controller"
)

var scheme = runtime.NewScheme()

func init() {
	_ = clientgoscheme.AddToScheme(scheme)
	_ = kubevirtv1.AddToScheme(scheme) // registers VirtualMachineInstance, VirtualMachine, VirtualMachineInstanceMigration
}

func main() {
	var (
		metricsAddr             string
		healthProbeAddr         string
		leaderElect             bool
		defaultDeadlineDuration time.Duration
	)

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "Address for the metrics endpoint.")
	flag.StringVar(&healthProbeAddr, "health-probe-bind-address", ":8081", "Address for the health probe endpoint.")
	flag.BoolVar(&leaderElect, "leader-elect", false, "Enable leader election.")
	flag.DurationVar(&defaultDeadlineDuration, "default-drain-deadline-duration", time.Hour, "Default deadline duration from evacuation start when no node annotation is set.")
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseDevMode(false)))
	setupLog := ctrl.Log.WithName("setup")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: healthProbeAddr,
		LeaderElection:         leaderElect,
		LeaderElectionID:       "deadline-eviction.kubevirt.io",
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	if err := (&controller.VMIEvictionReconciler{
		Client:                       mgr.GetClient(),
		DefaultDrainDeadlineDuration: defaultDeadlineDuration,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller")
		os.Exit(1)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}
