{
  description = "flox-controller — FloxEnv CRD + node-agent controller for runtime flox-env delivery";

  # Follow the seedmatic aggregator so the whole closure resolves to one nixpkgs
  # (same discipline as flox-nri-plugin / rke2lab runtime-flox).
  inputs = {
    flake-commons.url = "github:seedmatic/nix-flake-commons/develop";
    nixpkgs.follows = "flake-commons/nixpkgs";
    flake-utils.follows = "flake-commons/flake-utils";
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
            pname = "flox-controller";
            inherit version;
            src = ./.;
            # Scaffold placeholder: run `go mod tidy` + `nix build` once, then set
            # this to the hash nix prints (deterministic vendoring of the Go deps).
            vendorHash = pkgs.lib.fakeHash;
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
          # `seedmatic/flox-controller:<version>` with imagePullPolicy: IfNotPresent.
          # nix is NOT in the image: the controller execs the NODE's nix (host /nix
          # mounted in) to realise closures onto the host store.
          # NOTE: dockerTools can't build on darwin — build on the aarch64-linux
          # builder (`nix build .#flox-controller-image --system aarch64-linux --max-jobs 0`).
          flox-controller-image = pkgs.dockerTools.buildLayeredImage {
            name = "seedmatic/flox-controller";
            tag = version;
            contents = [ flox-controller pkgs.cacert ];
            config = {
              Entrypoint = [ "/bin/flox-controller" ];
              Env = [ "SSL_CERT_FILE=/etc/ssl/certs/ca-bundle.crt" ];
            };
          };

          default = flox-controller;
        };

        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            go
            gopls
            kubernetes-controller-tools # controller-gen (deepcopy + CRD)
            kubectl
          ];
        };
      });
}
