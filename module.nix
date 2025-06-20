{
  config,
  lib,
  pkgs,
  ...
}:
with lib;
let
  cfg = config.services.sync4loong;
  settingsFormat = pkgs.formats.toml { };
  configFile = settingsFormat.generate "sync4loong.toml" cfg.settings;
in
{
  options.services.sync4loong = {
    enable = mkEnableOption "sync4loong daemon";

    package = mkOption {
      type = types.package;
      default = pkgs.callPackage ./package.nix { };
      description = "The sync4loong package to use.";
    };

    settings = mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        Configuration for sync4loong daemon.

        S3 configuration is required for the daemon to function.
        SSH settings are optional but enable remote execution after sync.
      '';
    };

    user = mkOption {
      type = types.str;
      default = "sync4loong";
      description = "User account under which sync4loong runs.";
    };

    group = mkOption {
      type = types.str;
      default = "sync4loong";
      description = "Group under which sync4loong runs.";
    };

    dataDir = mkOption {
      type = types.path;
      default = "/var/lib/sync4loong";
      description = "Directory where sync4loong stores its data.";
    };
  };

  config = mkIf cfg.enable {
    users.users.${cfg.user} = {
      isSystemUser = true;
      group = cfg.group;
      home = cfg.dataDir;
      createHome = true;
      description = "sync4loong daemon user";
    };

    users.groups.${cfg.group} = { };

    systemd.services.sync4loong = {
      description = "Sync4loong daemon - File synchronization to S3";

      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        Type = "exec";
        User = cfg.user;
        Group = cfg.group;
        WorkingDirectory = cfg.dataDir;

        NoNewPrivileges = true;
        PrivateTmp = false;
        ProtectSystem = "strict";
        ProtectHome = true;
        ReadWritePaths = [ cfg.dataDir ];

        Restart = "always";
        RestartSec = "10s";

        StandardOutput = "journal";
        StandardError = "journal";
        SyslogIdentifier = "sync4loong";

        LimitNOFILE = 65536;

        Environment = [
          "SYNC4LOONG_CONFIG=${configFile}"
        ];
      };

      script = ''
        exec ${cfg.package}/bin/sync4loong-daemon --config ${configFile}
      '';

      preStart = ''
        mkdir -p ${cfg.dataDir}
        chown ${cfg.user}:${cfg.group} ${cfg.dataDir}
        chmod 755 ${cfg.dataDir}
      '';
    };

    environment.systemPackages = [
      cfg.package
      (pkgs.writeShellScriptBin "sync4loong-publish" ''
        exec ${cfg.package}/bin/sync4loong-publish --config ${configFile} "$@"
      '')
    ];
  };
}
