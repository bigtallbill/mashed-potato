{
  description = "mashed-potato — single-binary restic backup manager (daemon: mashed-potatod)";

  inputs.nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";

  outputs = { self, nixpkgs }:
    let
      systems = [ "x86_64-linux" "aarch64-linux" ];
      forAllSystems = f: nixpkgs.lib.genAttrs systems (system:
        f (import nixpkgs { inherit system; }));
    in
    {
      packages = forAllSystems (pkgs: rec {
        # Single source of truth shared with channels-based configs (see nix/).
        mashed-potato = pkgs.callPackage ./nix/package.nix { };
        default = mashed-potato;
      });

      apps = forAllSystems (pkgs: rec {
        mashed-potatod = {
          type = "app";
          program = "${self.packages.${pkgs.stdenv.hostPlatform.system}.mashed-potato}/bin/mashed-potatod";
        };
        default = mashed-potatod;
      });

      devShells = forAllSystems (pkgs: {
        default = pkgs.mkShell {
          packages = [
            pkgs.go
            pkgs.gopls
            pkgs.gotools # goimports
            pkgs.restic # so `mashed-potatod run ...` works inside the dev shell
          ];
        };
      });

      formatter = forAllSystems (pkgs: pkgs.nixpkgs-fmt);

      # Same module file used by channels-based configs (import by path).
      nixosModules.mashed-potato = ./nix/module.nix;
      nixosModules.default = ./nix/module.nix;
    };
}
