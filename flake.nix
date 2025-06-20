{
  description = "sync4loong";

  inputs = {
    nixpkgs.url = "https://seele.gg/https://github.com/loongson-community/nixpkgs/archive/7da462cc6ad05d7ed17bae930ceb86052ab34c50.zip";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    let
      version = if self ? rev then "git-${builtins.substring 0 7 self.rev}" else "dev";

      supportedSystems = [
        "x86_64-linux"
        "aarch64-linux"
        "x86_64-darwin"
        "aarch64-darwin"
        "loongarch64-linux"
      ];
    in
    flake-utils.lib.eachSystem supportedSystems (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages = {
          default = self.packages.${system}.sync4loong;

          sync4loong = pkgs.callPackage ./package.nix {
            inherit version;
            vendorHash = "sha256-rLU3igRza16+YLfnVBjl+0Rh5K2yekLjcKpvqZJy8bY=";
            src = self;
          };
        };

        apps = {
          default = self.apps.${system}.daemon;

          daemon = flake-utils.lib.mkApp {
            drv = self.packages.${system}.sync4loong;
            exePath = "/bin/sync4loong-daemon";
          };

          publish = flake-utils.lib.mkApp {
            drv = self.packages.${system}.sync4loong;
            exePath = "/bin/sync4loong-publish";
          };
        };
      }
    )
    // {
      nixosModules.default = import ./module.nix;

      overlays.default = final: prev: {
        sync4loong = final.callPackage ./package.nix {
          inherit version;
          src = self;
        };
      };
    };
}
