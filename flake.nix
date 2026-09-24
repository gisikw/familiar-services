{
  description = "familiar-services — Familiar's singleton layer";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let pkgs = import nixpkgs { inherit system; };
      in {
        packages.default = pkgs.buildGoModule {
          pname = "familiar-services";
          version = if (self ? shortRev) then self.shortRev else "dev";
          src = ./.;
          vendorHash = null;
        };
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [ go gopls sqlite ];
        };
      });
}
