{ self }:
{ config, lib, pkgs, ... }:
let
  cfg = config.services.familiar-continuity;
  dbDir = builtins.dirOf cfg.dbPath;
  package = self.packages.${pkgs.stdenv.hostPlatform.system}.default;
  args = lib.escapeShellArgs [
    "continuity" "import"
    "--sessions" cfg.sessionsDir
    "--handoffs" cfg.handoffsDir
    "--db" cfg.dbPath
  ];
in {
  options.services.familiar-continuity = {
    enable = lib.mkEnableOption "the Familiar read-only continuity mirror";
    sessionsDir = lib.mkOption {
      type = lib.types.str;
      description = "Directory containing Pi session JSONL files.";
    };
    handoffsDir = lib.mkOption {
      type = lib.types.str;
      description = "Directory containing timestamp-named handoff Markdown files.";
    };
    dbPath = lib.mkOption {
      type = lib.types.str;
      default = "/var/lib/familiar-continuity/continuity.db";
      description = "Path to the disposable continuity SQLite index.";
    };
    user = lib.mkOption {
      type = lib.types.str;
      default = "familiar";
      description = "User which runs the importer.";
    };
    group = lib.mkOption {
      type = lib.types.str;
      default = "familiar";
      description = "Group which runs the importer.";
    };
  };

  config = lib.mkIf cfg.enable {
    systemd.tmpfiles.rules = [ "d ${dbDir} 0750 ${cfg.user} ${cfg.group} -" ];

    systemd.services.familiar-continuity-import = {
      description = "Catch up the Familiar continuity mirror";
      serviceConfig = {
        Type = "oneshot";
        User = cfg.user;
        Group = cfg.group;
        ExecStart = "${package}/bin/familiar-services ${args}";
        UMask = "0027";
        NoNewPrivileges = true;
        PrivateDevices = true;
        PrivateTmp = true;
        ProtectClock = true;
        ProtectControlGroups = true;
        ProtectHome = "read-only";
        ProtectHostname = true;
        ProtectKernelLogs = true;
        ProtectKernelModules = true;
        ProtectKernelTunables = true;
        ProtectSystem = "strict";
        RestrictAddressFamilies = [ "AF_UNIX" ];
        RestrictNamespaces = true;
        RestrictRealtime = true;
        LockPersonality = true;
        MemoryDenyWriteExecute = true;
        RemoveIPC = true;
        ReadOnlyPaths = [ cfg.sessionsDir cfg.handoffsDir ];
        ReadWritePaths = [ dbDir ];
      };
    };

    systemd.paths.familiar-continuity-import = {
      description = "Watch Pi sessions for continuity catch-up";
      wantedBy = [ "paths.target" ];
      pathConfig = {
        PathChanged = cfg.sessionsDir;
        Unit = "familiar-continuity-import.service";
      };
    };

    systemd.timers.familiar-continuity-import = {
      description = "Periodic Familiar continuity catch-up";
      wantedBy = [ "timers.target" ];
      timerConfig = {
        OnBootSec = "15min";
        OnUnitActiveSec = "15min";
        Persistent = true;
        Unit = "familiar-continuity-import.service";
      };
    };
  };
}
