{
  lib,
  buildGoModule,
  pname ? "sync4loong",
  version ? "dev",
  src ? ./.,
  vendorHash ? null,
}:
let
  gitCommit =
    if builtins.pathExists "${src}/.git" then builtins.readFile "${src}/.git/HEAD" else "unknown";
in
buildGoModule {
  inherit
    pname
    version
    src
    vendorHash
    ;

  subPackages = [
    "cmd/daemon"
    "cmd/publish"
  ];

  ldflags = [
    "-s"
    "-w"
    "-X main.version=${version}"
    "-X main.commit=${gitCommit}"
  ];

  patchPhase = ''
    find . -name "*_test.go" -delete
  '';

  postInstall = ''
    # Rename binaries to match expected names
    mv $out/bin/daemon $out/bin/sync4loong-daemon
    mv $out/bin/publish $out/bin/sync4loong-publish

    # Install example config
    mkdir -p $out/share/sync4loong
    cp config.example.toml $out/share/sync4loong/
  '';

  meta = with lib; {
    description = "A file synchronization system based on Go and Asynq for syncing local folders to S3 storage";
    homepage = "https://github.com/darkyzhou/sync4loong";
    license = licenses.mit;
    maintainers = [ ];
    platforms = platforms.linux ++ platforms.darwin;
    mainProgram = "sync4loong-daemon";
  };
}
