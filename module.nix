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

    package = lib.mkPackageOption pkgs.system "sync4loong" { default = "sync4loong"; };

    settings = mkOption {
      type = settingsFormat.type;
      default = { };
      description = ''
        Configuration for sync4loong daemon.

        S3 configuration is required for the daemon to function.
        SSH settings are optional but enable remote execution after sync.
        HTTP configuration is optional (defaults to :8080).
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

    mkUser = mkOption {
      type = types.bool;
      default = true;
      description = "Whether to create the sync4loong user account.";
    };

    mkGroup = mkOption {
      type = types.bool;
      default = true;
      description = "Whether to create the sync4loong group.";
    };
  };

  config = mkIf cfg.enable {
    users.users = mkIf cfg.mkUser {
      ${cfg.user} = {
        isSystemUser = true;
        group = cfg.group;
        description = "sync4loong daemon user";
      };
    };

    users.groups = mkIf cfg.mkGroup {
      ${cfg.group} = { };
    };

    systemd.services.sync4loong = {
      description = "Sync4loong daemon - File synchronization to S3 with HTTP API";

      wantedBy = [ "multi-user.target" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];

      serviceConfig = {
        Type = "exec";
        User = cfg.user;
        Group = cfg.group;

        NoNewPrivileges = true;
        PrivateTmp = false;
        ProtectSystem = "strict";
        ProtectHome = true;

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
    };
  };
}
