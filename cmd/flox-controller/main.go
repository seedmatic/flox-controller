package main

import (
	"context"
	"flag"
	"os"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/manager"

	floxv1alpha1 "github.com/seedmatic/flox-controller/api/v1alpha1"
	"github.com/seedmatic/flox-controller/internal/carrier"
	"github.com/seedmatic/flox-controller/internal/controller"
	"github.com/seedmatic/flox-controller/internal/provisioner"
)

var (
	scheme  = runtime.NewScheme()
	version = "dev"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(floxv1alpha1.AddToScheme(scheme))
}

func main() {
	var probeAddr, envRoot, gcrootBase, ctrBin, containerdAddress, baseCarrierNamespace string
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.StringVar(&envRoot, "env-root", "/var/lib/flox-controller/envs",
		"host dir where .flox env sources materialise (<env-root>/<folder>/<name>)")
	flag.StringVar(&gcrootBase, "gcroot-base", "/nix/var/nix/gcroots/flox-runtime/env",
		"flox-runtime GC-root dir the NRI plugin reads")
	flag.StringVar(&ctrBin, "ctr-bin", "/var/lib/rancher/rke2/bin/ctr",
		"containerd CLI used to import carrier images (rke2 ships it here, off the default PATH)")
	flag.StringVar(&containerdAddress, "containerd-address", "/run/k3s/containerd/containerd.sock",
		"containerd socket ctr imports into (rke2's k3s-containerd)")
	// The controller owns its base carrier: it self-provisions it (see internal/carrier).
	// Defaults to the controller's own namespace (always exists), else flox-system.
	defaultCarrierNs := os.Getenv("POD_NAMESPACE")
	if defaultCarrierNs == "" {
		defaultCarrierNs = "flox-system"
	}
	flag.StringVar(&baseCarrierNamespace, "base-carrier-namespace", defaultCarrierNs,
		"namespace for the controller's embedded base carrier FloxEnv")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting flox-controller", "version", version)

	// Node-agent: this pod reconciles its own node (NODE_NAME via the Downward API).
	nodeName := os.Getenv("NODE_NAME")

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		HealthProbeBindAddress: probeAddr,
	})
	if err != nil {
		setupLog.Error(err, "unable to create manager")
		os.Exit(1)
	}

	if err := (&controller.FloxEnvReconciler{
		Client:   mgr.GetClient(),
		NodeName: nodeName,
		Provisioner: &provisioner.ExecProvisioner{
			EnvRoot:           envRoot,
			GcrootBase:        gcrootBase,
			CtrBin:            ctrBin,
			ContainerdAddress: containerdAddress,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller", "controller", "FloxEnv")
		os.Exit(1)
	}

	// Self-provision the controller's own base carrier once the cache is up. Best-effort:
	// a failure (e.g. namespace not yet present) logs and is retried on restart, never
	// crash-loops the manager.
	if err := mgr.Add(manager.RunnableFunc(func(ctx context.Context) error {
		if err := carrier.EnsureBase(ctx, mgr.GetClient(), baseCarrierNamespace); err != nil {
			setupLog.Error(err, "base carrier ensure failed (will retry on restart)")
		}
		return nil
	})); err != nil {
		setupLog.Error(err, "unable to register base-carrier ensurer")
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
		setupLog.Error(err, "manager exited")
		os.Exit(1)
	}
}
