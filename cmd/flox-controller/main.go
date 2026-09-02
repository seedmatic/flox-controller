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
	floxwebhook "github.com/seedmatic/flox-controller/internal/webhook"
)

var (
	scheme  = runtime.NewScheme()
	version = "dev"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(floxv1alpha1.AddToScheme(scheme))
}

// inSeparateMountNamespaceFromInit reports whether this process runs in a mount namespace
// distinct from PID 1's — i.e. inside a container, so host tools need nsenter to reach. With
// the DaemonSet's hostPID, /proc/1 is the node's init: a pod sees a different mnt ns (true),
// a bare host process (the nix-run) sees the same (false). Unreadable /proc ⇒ assume host.
func inSeparateMountNamespaceFromInit() bool {
	self, err1 := os.Readlink("/proc/self/ns/mnt")
	init, err2 := os.Readlink("/proc/1/ns/mnt")
	if err1 != nil || err2 != nil {
		return false
	}
	return self != init
}

func main() {
	var probeAddr, envRoot, gcrootBase, ctrBin, containerdAddress, nsenterBin, baseCarrierNamespace string
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "health probe endpoint")
	flag.StringVar(&envRoot, "env-root", "/var/lib/flox-controller/envs",
		"host dir where .flox env sources materialise (<env-root>/<folder>/<name>)")
	flag.StringVar(&gcrootBase, "gcroot-base", "/nix/var/nix/gcroots/flox-runtime/env",
		"flox-runtime GC-root dir the NRI plugin reads")
	flag.StringVar(&ctrBin, "ctr-bin", "/var/lib/rancher/rke2/bin/ctr",
		"containerd CLI used to import carrier images (rke2 ships it here, off the default PATH)")
	flag.StringVar(&containerdAddress, "containerd-address", "/run/k3s/containerd/containerd.sock",
		"containerd socket ctr imports into (rke2's k3s-containerd)")
	flag.StringVar(&nsenterBin, "nsenter-bin", "/usr/local/bin/nsenter",
		"nsenter used to reach host tools when containerized (baked real-file); empty disables")
	// The controller owns its base carrier: it self-provisions it (see internal/carrier).
	// Defaults to the controller's own namespace (always exists), else flox-system.
	defaultCarrierNs := os.Getenv("POD_NAMESPACE")
	if defaultCarrierNs == "" {
		defaultCarrierNs = "flox-system"
	}
	flag.StringVar(&baseCarrierNamespace, "base-carrier-namespace", defaultCarrierNs,
		"namespace for the controller's embedded base carrier FloxEnv")
	var enableWebhook bool
	var waitImage, tokenSecretName, tokenSecretKey string
	var nixStoreClass, nixStoreSize string
	flag.BoolVar(&enableWebhook, "enable-webhook", false,
		"serve the pod-mutating webhook (needs TLS certs at the webhook cert dir) — a Service fronts the DaemonSet pods; the mutation is stateless so any pod serves")
	flag.StringVar(&waitImage, "flox-wait-image", "busybox:stable",
		"image for the injected flox-wait init container (needs /bin/sh)")
	flag.StringVar(&tokenSecretName, "token-secret-name", "",
		"name of the (replicated) Secret carrying the FloxHub token; empty disables token injection")
	flag.StringVar(&tokenSecretKey, "token-secret-key", "token",
		"key within the token secret holding the FloxHub token value")
	flag.StringVar(&nixStoreClass, "nix-store-class", "openebs-zfs-shared",
		"StorageClass for the ensured nix-store PVC; empty uses the cluster default")
	flag.StringVar(&nixStoreSize, "nix-store-size", "30Gi",
		"requested size of the ensured nix-store PVC")
	opts := zap.Options{Development: true}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))
	setupLog := ctrl.Log.WithName("setup")
	setupLog.Info("starting flox-controller", "version", version)

	// Node-agent: this pod reconciles its own node (NODE_NAME via the Downward API).
	nodeName := os.Getenv("NODE_NAME")

	// One binary, two contexts: run directly on the node (the nix-run — host tools found
	// natively) or inside the DaemonSet (host tools reachable only via nsenter into PID 1's
	// namespaces). Auto-detect by comparing our mount namespace to the host init's (needs
	// hostPID, which the DaemonSet sets); same ns ⇒ we ARE the host ⇒ exec directly.
	nsenter := ""
	if inSeparateMountNamespaceFromInit() {
		nsenter = nsenterBin
		setupLog.Info("host tools via nsenter (containerized)", "nsenter", nsenter)
	} else {
		setupLog.Info("host tools exec'd directly (running on the node)")
	}

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
			Nsenter:           nsenter,
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller", "controller", "FloxEnv")
		os.Exit(1)
	}

	// FloxCatalog resolves a nix-flake catalog from a Flux source's reconciled artifact
	// (cluster-scoped; on multi-node it runs redundantly on each DaemonSet pod — accepted,
	// the resolution is idempotent; leader election is a future optimisation, not required).
	// It needs Flux's GitRepository CRD present.
	if err := (&controller.FloxCatalogReconciler{
		Client: mgr.GetClient(),
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to set up controller", "controller", "FloxCatalog")
		os.Exit(1)
	}

	// The pod-mutating webhook (--enable-webhook) injects the node-aware flox-wait barrier +
	// the flox env/token into flox-annotated pods. It runs in every DaemonSet pod behind a
	// Service (stateless mutation → any pod serves); serving needs the TLS cert mounted at the
	// webhook cert dir. The FloxHub token is injected valueFrom the replicated token Secret.
	if enableWebhook {
		if err := (&floxwebhook.PodFloxMutator{
			GcrootBase:      gcrootBase,
			WaitImage:       waitImage,
			TimeoutSeconds:  120,
			TokenSecretName: tokenSecretName,
			TokenSecretKey:  tokenSecretKey,
			Client:        mgr.GetClient(),
			NixStoreClass: nixStoreClass,
			NixStoreSize:  nixStoreSize,
		}).SetupWebhookWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to set up webhook", "webhook", "PodFloxWait")
			os.Exit(1)
		}
		setupLog.Info("pod flox-wait mutating webhook enabled")
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
