{
  description = "flox-controller — FloxEnv CRD + node-agent controller for runtime flox-env delivery";

  # Public flake: consumers outside the fleet lack ndh's system-wide cache-trust, so declare the
  # non-default binary caches this flake's closure resolves from. ADDITIVE (extra-*) + public-read,
  # honoured via --accept-flake-config — an external clone or CI substitutes instead of rebuilding.
  #   - nxmatic.cachix.org : where our build of the controller is published (substituted, not rebuilt).
  #   - cache.flox.dev     : this flake builds against flake-commons' nixpkgs and lives in the flox
  #                          ecosystem (its closure pulls flox/nixpkgs fork nodes), whose store paths
  #                          are on flox's cache rather than cache.nixos.org.
  nixConfig.extra-substituters = [
    "https://cache.flox.dev"
    "https://nxmatic.cachix.org"
  ];
  nixConfig.extra-trusted-public-keys = [
    "flox-cache-public-1:7F4OyH7ZCnFhcze3fJdfyXYLQw/aV7GEed86nQ7IsOs="
    "nxmatic.cachix.org-1:huMghYiwDpPa1PMXHXK4G1Dp4QOZjgsNqxcjf/AjuJ0="
  ];

  # Follow the seedmatic aggregator so the whole closure resolves to one nixpkgs
  # (same discipline as flox-nri-plugin / rke2lab runtime-flox).
  # Every SEEDMATIC-owned input below is an INDIRECT id (`url = "flake-commons"`), resolved through
  # nix's registry: the branch-less default target lives in the committed flake-registry.json, and
  # the operator re-aims it by dropping a flake-registry.local.json beside it (see the [include] in
  # .flox/env/manifest.toml). A branch named here could only be re-aimed by pushing an edit to this
  # file; naming none means naming nothing that can be deleted. The lock still records a revision,
  # so evaluating from it needs no registry at all.
  inputs = {
    flake-commons.url = "flake-commons";
    nixpkgs.follows = "flake-commons/nixpkgs";
    flake-utils.follows = "flake-commons/flake-utils";

    # This flake consumes ONLY flake-commons' nixpkgs + flake-utils (a Go build). Its other ~26
    # transitive inputs (a full darwin/home-manager/browser fleet) would otherwise drag their whole
    # closures into our lock — ~12k nodes / 8.6 MB. Redirect every unused one to nixpkgs so the
    # subtrees collapse out of the lock (the flox-nri-plugin idiom → a ~5-node lock). When rke2lab
    # consumes this flake it overrides flake-commons anyway; this keeps the STANDALONE lock lean for
    # external clones / CI. Keep this list in sync if the aggregator grows.
    flake-commons.inputs.bird.follows = "nixpkgs";
    flake-commons.inputs.chromium-bin.follows = "nixpkgs";
    flake-commons.inputs.darwin.follows = "nixpkgs";
    flake-commons.inputs.determinate.follows = "nixpkgs";
    flake-commons.inputs.disko.follows = "nixpkgs";
    flake-commons.inputs.extra-container.follows = "nixpkgs";
    flake-commons.inputs.flake-compat.follows = "nixpkgs";
    flake-commons.inputs.flox.follows = "nixpkgs";
    flake-commons.inputs.home-manager.follows = "nixpkgs";
    flake-commons.inputs.impermanence.follows = "nixpkgs";
    flake-commons.inputs.lix-module.follows = "nixpkgs";
    flake-commons.inputs.maven-mvnd.follows = "nixpkgs";
    flake-commons.inputs.nix.follows = "nixpkgs";
    flake-commons.inputs.nix-snapshotter.follows = "nixpkgs";
    flake-commons.inputs.nixos-generators.follows = "nixpkgs";
    flake-commons.inputs.nixos-hardware.follows = "nixpkgs";
    flake-commons.inputs.nixpkgs-unstable.follows = "nixpkgs";
    flake-commons.inputs.nvfetcher.follows = "nixpkgs";
    flake-commons.inputs.sops-nix.follows = "nixpkgs";
    flake-commons.inputs.treefmt-nix.follows = "nixpkgs";
  };

  outputs = inputs@{ self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        version = pkgs.lib.fileContents ./VERSION;
      in
      {
        packages = rec {
          # The controller binary. Static (CGO off) so it runs in a minimal image.
          flox-controller = pkgs.buildGoModule {
            # Store-name prefix io.seedmatic.<asset> (org convention, was io.nxmatic
            # before the seedmatic move) so the artifact is findable in /nix/store;
            # meta.mainProgram keeps the bin at bin/flox-controller for `nix run`.
            pname = "io.seedmatic.flox-controller";
            inherit version;
            src = ./.;
            # Deterministic vendoring of the Go deps. Regenerate after a go.mod change:
            # set to lib.fakeHash, `nix build`, then paste the hash nix prints.
            vendorHash = "sha256-TRKTDXhNP6NNtH0YNWy2tTzhZ6XeQOtgoEfPJk44Gsw=";
            subPackages = [ "cmd/flox-controller" ];
            env.CGO_ENABLED = 0;
            ldflags = [ "-s" "-w" "-X main.version=${version}" ];
            meta.mainProgram = "flox-controller";
          };

          # OCI image for the node-agent DaemonSet — the DISTRIBUTION artifact.
          # Air-gap path (as for flox-carrier): the consumer (rke2lab) bakes this tar
          # into the node-base image via a tmpfiles symlink into
          # /var/lib/rancher/rke2/agent/images/, rke2 auto-imports it into local
          # containerd at boot, and the DaemonSet references it by its exact RepoTag
          # with imagePullPolicy: IfNotPresent.
          #
          # ★ NO EXPLICIT TAG, deliberately: omitted, dockerTools tags the image with its
          # OUTPUT HASH, so the tag MOVES WITH THE CONTENT.  It used to be `tag = version`,
          # and `version` is a static VERSION file (`0.0.0-develop`) — so every rebuild
          # produced different content under the SAME RepoTag.  Combined with the air-gap
          # path (baked tar + auto-import) and `IfNotPresent`, a node that already held
          # that tag NEVER adopted a new build: the new binary shipped inside the node
          # image and the pod kept running the old one, with nothing reporting it.  A
          # content-derived tag makes `IfNotPresent` correct instead of a trap — new
          # content is a new tag the node cannot already have, and the old tag stays
          # available for a rollback.
          #
          # Consumers must therefore READ the RepoTag (see flox-controller-image-ref)
          # rather than restate it: a literal `name:version` on the consumer side cannot
          # be right any more, which is the point.
          # nix is NOT in the image: the controller execs the NODE's nix (host /nix
          # mounted in) to realise closures onto the host store.
          # NOTE: dockerTools can't build on darwin — build on the aarch64-linux
          # builder (`nix build .#flox-controller-image --system aarch64-linux --max-jobs 0`).
          flox-controller-image = pkgs.dockerTools.buildLayeredImage {
            # OCI name doubles as the store-path basename → same io.seedmatic.<asset>
            # prefix for store discoverability + a self-evident image ref.
            name = "io.seedmatic.flox-controller";
            # The controller mounts the HOST /nix at /nix (to realise closures + GC-roots and
            # exec the node's flox/nix/ctr) — which SHADOWS the image's own /nix/store. So its
            # binary + cacert must be REAL files OUTSIDE /nix, else the entrypoint (and any
            # /nix/store path) dangles under the mount — the chicken-and-egg. Same real-file
            # lesson as the flox carrier's /etc. The Go binary is static (CGO off) → a plain copy
            # runs standalone; it needs the mounted /nix only to FIND flox/nix/ctr on PATH.
            extraCommands = ''
              mkdir -p usr/local/bin etc/ssl/certs
              cp ${flox-controller}/bin/flox-controller usr/local/bin/flox-controller
              cp ${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt etc/ssl/certs/ca-bundle.crt
              # STATIC nsenter (real file, not shadowed by the host /nix overlay): the controller
              # execs it to reach the node's flox/nix/ctr from inside the DaemonSet. A dynamic
              # nsenter would dangle under the mount, like a store-path binary would.
              cp ${pkgs.pkgsStatic.util-linux}/bin/nsenter usr/local/bin/nsenter
            '';
            config = {
              Entrypoint = [ "/usr/local/bin/flox-controller" ];
              Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" ];
            };
          };

          # The image's RepoTag as a store artifact — the SINGLE SOURCE of the reference
          # the consumer's DaemonSet must carry, published because the tag is now derived
          # from the image's content and so cannot be written down anywhere by hand.
          #
          # Read off the image's own passthru, never recomputed: the ref and the image it
          # names cannot disagree.  Shaped as a directory with one file, like the CRD and
          # RBAC artifacts, so the consumer stages all three the same way.
          flox-controller-image-ref =
            pkgs.runCommand "io.seedmatic.flox-controller-image-ref" { } ''
              mkdir -p "$out"
              printf '%s' "${flox-controller-image.imageName}:${flox-controller-image.imageTag}" \
                > "$out/flox-controller"
            '';

          # The generated CRD(s) as a store artifact — the single source consumers
          # (rke2lab's manifest synthesis) emit into the cluster's `crds` layer,
          # instead of re-modelling the schema. Regenerated by `controller-gen crd`
          # (see the flox dev env); this just stages the committed output.
          flox-controller-crds = pkgs.runCommand "io.seedmatic.flox-controller-crds" { } ''
            mkdir -p "$out"
            cp ${./config/crd}/*.yaml "$out"/
          '';

          # The generated ClusterRole as a store artifact — the SINGLE SOURCE of the
          # controller's RBAC, derived from the `+kubebuilder:rbac` markers (regenerated
          # by `make rbac`). rke2lab's FloxControllerManifestsUnit INCLUDES this instead
          # of hand-listing the rules; it still authors the ServiceAccount +
          # ClusterRoleBinding + Deployment + webhook (its deployment-topology concern).
          flox-controller-rbac = pkgs.runCommand "io.seedmatic.flox-controller-rbac" { } ''
            mkdir -p "$out"
            cp ${./config/rbac}/*.yaml "$out"/
          '';

          default = flox-controller;
        };

        # relock — THIS repo's locks, by the shared implementation in nix-flake-commons'
        # `lib.mkRelockApp`: bump each input, drop any bump that moves no exported derivation, push.
        apps.relock = {
          type = "app";
          program = "${
            inputs.flake-commons.lib.mkRelockApp {
              inherit pkgs;
              name = "flox-controller";
              slug = "seedmatic/flox-controller";
              url = "https://github.com/seedmatic/flox-controller.git";
              consumers = [ "github:seedmatic/rke2lab" ];
            }
          }/bin/relock";
          meta.description = "Reconcile THIS repo's locks: bump each input, DROP any bump that moves no exported derivation, push — impl: nix-flake-commons lib.mkRelockApp";
        };

        # Dev toolchain lives in the flox env (.flox/env/manifest.toml): `flox activate`
        # provides go, controller-gen, kubectl, delve, make.
      });
}
