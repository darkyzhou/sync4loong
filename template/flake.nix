{
  description = "Sync4loong deployment example";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    sync4loong.url = "github:darkyzhou/sync4loong";
  };

  outputs = { self, nixpkgs, sync4loong }:
    {
      nixosConfigurations.example = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        modules = [
          sync4loong.nixosModules.default
          {
            services.sync4loong = {
              enable = true;
              settings = {
                s3 = {
                  endpoint = "https://s3.amazonaws.com";
                  region = "us-east-1"; 
                  bucket = "your-bucket-name";
                  access_key = "your-access-key";
                  secret_key = "your-secret-key";
                };
                daemon = {
                  concurrency = 4;
                  ssh_command = "ssh user@host 'sudo nixos-rebuild switch'";
                  ssh_debounce_minutes = 5;
                  ssh_timeout_minutes = 10;
                  enable_ssh_task = true;
                };
              };
              openFirewall = true;
            };
          }
        ];
      };
    };
}