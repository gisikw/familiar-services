{
  description = "familiar-services — Familiar's singleton layer";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils, ... }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = import nixpkgs { inherit system; };
        moduleEval = nixpkgs.lib.nixosSystem {
          inherit system;
          modules = [
            self.nixosModules.default
            {
              services.familiar-continuity = {
                enable = true;
                sessionsDir = "/srv/pi/sessions";
                handoffsDir = "/srv/pi/handoffs";
              };
            }
          ];
        };
      in {
        packages.default = pkgs.buildGoModule {
          pname = "familiar-services";
          version = if (self ? shortRev) then self.shortRev else "dev";
          src = ./.;
          vendorHash = null;
        };
        checks.module-eval = pkgs.runCommand "familiar-continuity-module-eval" {
          service = moduleEval.config.systemd.services.familiar-continuity-import.serviceConfig.ExecStart;
          pathUnit = builtins.toJSON moduleEval.config.systemd.paths.familiar-continuity-import.pathConfig;
          timerUnit = builtins.toJSON moduleEval.config.systemd.timers.familiar-continuity-import.timerConfig;
        } ''
          test -n "$service"
          test -n "$pathUnit"
          test -n "$timerUnit"
          touch $out
        '';
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [ go gopls sqlite ];
        };
      }) // {
        nixosModules.default = import ./nix/module.nix { inherit self; };
      };
}
