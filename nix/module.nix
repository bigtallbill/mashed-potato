# NixOS module — runs `mashed-potatod serve` as a *user* service so the tray icon
# works in your graphical session and it can manage per-job user timers. No
# home-manager required: import this by path from configuration.nix, e.g.
#
#   imports = [ /path/to/mashed-potato/nix/module.nix ];
#   services.mashed-potato.enable = true;
{ config, lib, pkgs, ... }:

let cfg = config.services.mashed-potato;
in {
  options.services.mashed-potato = {
    enable = lib.mkEnableOption "mashed-potato backup manager (web UI + tray) as a user service";

    package = lib.mkOption {
      type = lib.types.package;
      default = pkgs.callPackage ./package.nix { };
      defaultText = lib.literalExpression "pkgs.callPackage ./package.nix { }";
      description = "The mashed-potato package to run.";
    };

    addr = lib.mkOption {
      type = lib.types.str;
      default = "127.0.0.1:8765";
      description = "Web UI listen address. Keep it on loopback.";
    };

    tray = lib.mkOption {
      type = lib.types.bool;
      default = true;
      description = "Show the system-tray icon (needs a graphical session).";
    };

    extraArgs = lib.mkOption {
      type = lib.types.listOf lib.types.str;
      default = [ ];
      description = "Extra arguments appended to `mashed-potatod serve`.";
    };
  };

  config = lib.mkIf cfg.enable {
    environment.systemPackages = [ cfg.package ]; # CLI: init-repo, enable, run, …

    systemd.user.services.mashed-potato = {
      description = "mashed-potato backup manager (web UI + tray)";
      wantedBy = [ "graphical-session.target" ];
      partOf = [ "graphical-session.target" ];
      after = [ "graphical-session.target" ];
      path = [ pkgs.xdg-utils ]; # for "Open dashboard" (xdg-open)
      serviceConfig = {
        ExecStart = lib.escapeShellArgs (
          [ "${cfg.package}/bin/mashed-potatod" "serve" "--addr" cfg.addr ]
          ++ lib.optional (!cfg.tray) "--no-tray"
          ++ cfg.extraArgs
        );
        Restart = "on-failure";
        RestartSec = 5;
      };
    };
  };
}
