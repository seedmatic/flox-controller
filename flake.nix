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
          flox-controller = pkgs.buildGoModule {
            pname = "flox-controller";
            inherit version;
            src = ./.;
            # Scaffold placeholder: run `go mod tidy` + `nix build` once, then set
            # this to the hash nix prints (deterministic vendoring of the Go deps).
            vendorHash = pkgs.lib.fakeHash;
            subPackages = [ "cmd/flox-controller" ];
            ldflags = [ "-s" "-w" "-X main.version=${version}" ];
            meta.mainProgram = "flox-controller";
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
