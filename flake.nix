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
  inputs = {
    flake-commons.url = "github:seedmatic/nix-flake-commons/develop";
    nixpkgs.follows = "flake-commons/nixpkgs";
    flake-utils.follows = "flake-commons/flake-utils";

    # This flake consumes ONLY flake-commons' nixpkgs + flake-utils (a Go build). Its other ~26
    # transitive inputs (a full darwin/home-manager/browser fleet) would otherwise drag their whole
    # closures into our lock — ~12k nodes / 8.6 MB. Redirect every unused one to nixpkgs so the
    # subtrees collapse out of the lock (the flox-nri-plugin idiom → a ~5-node lock). When rke2lab
    # consumes this flake it overrides flake-commons anyway; this keeps the STANDALONE lock lean for
    # external clones / CI. Keep this list in sync if the aggregator grows.
    flake-commons.inputs.bird.follows = "nixpkgs";
    flake-commons.inputs.cachix.follows = "nixpkgs";
    flake-commons.inputs.chromium-bin.follows = "nixpkgs";
    flake-commons.inputs.darwin.follows = "nixpkgs";
    flake-commons.inputs.determinate.follows = "nixpkgs";
    flake-commons.inputs.devenv.follows = "nixpkgs";
    flake-commons.inputs.disko.follows = "nixpkgs";
    flake-commons.inputs.extra-container.follows = "nixpkgs";
    flake-commons.inputs.flake-compat.follows = "nixpkgs";
    flake-commons.inputs.flox.follows = "nixpkgs";
    flake-commons.inputs.home-manager.follows = "nixpkgs";
    flake-commons.inputs.impermanence.follows = "nixpkgs";
    flake-commons.inputs.incus-compose.follows = "nixpkgs";
    flake-commons.inputs.lix-module.follows = "nixpkgs";
    flake-commons.inputs.maven-mvnd.follows = "nixpkgs";
    flake-commons.inputs.nix.follows = "nixpkgs";
    flake-commons.inputs.nix-snapshotter.follows = "nixpkgs";
    flake-commons.inputs.nixos-generators.follows = "nixpkgs";
    flake-commons.inputs.nixos-hardware.follows = "nixpkgs";
    flake-commons.inputs.nixpkgs-unstable.follows = "nixpkgs";
    flake-commons.inputs.nvfetcher.follows = "nixpkgs";
    flake-commons.inputs.ripvcs.follows = "nixpkgs";
    flake-commons.inputs.socket-vmnet.follows = "nixpkgs";
    flake-commons.inputs.sops-nix.follows = "nixpkgs";
    flake-commons.inputs.treefmt-nix.follows = "nixpkgs";
    flake-commons.inputs.zen-browser.follows = "nixpkgs";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
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
          # containerd at boot, and the DaemonSet references
          # `io.seedmatic.flox-controller:<version>` with imagePullPolicy: IfNotPresent.
          # nix is NOT in the image: the controller execs the NODE's nix (host /nix
          # mounted in) to realise closures onto the host store.
          # NOTE: dockerTools can't build on darwin — build on the aarch64-linux
          # builder (`nix build .#flox-controller-image --system aarch64-linux --max-jobs 0`).
          flox-controller-image = pkgs.dockerTools.buildLayeredImage {
            # OCI name doubles as the store-path basename → same io.seedmatic.<asset>
            # prefix for store discoverability + a self-evident image ref.
            name = "io.seedmatic.flox-controller";
            tag = version;
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

          # The generated CRD(s) as a store artifact — the single source consumers
          # (rke2lab's manifest synthesis) emit into the cluster's `crds` layer,
          # instead of re-modelling the schema. Regenerated by `controller-gen crd`
          # (see the flox dev env); this just stages the committed output.
          flox-controller-crds = pkgs.runCommand "io.seedmatic.flox-controller-crds" { } ''
            mkdir -p "$out"
            cp ${./config/crd}/*.yaml "$out"/
          '';

          default = flox-controller;
        };

        # Dev toolchain lives in the flox env (.flox/env/manifest.toml): `flox activate`
        # provides go, controller-gen, kubectl, delve, make.
      });
}
