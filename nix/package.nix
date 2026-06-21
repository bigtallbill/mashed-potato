# Standalone package definition, usable from both the flake and a channels-based
# configuration.nix (via `pkgs.callPackage ./nix/package.nix { }`).
{ lib, buildGoModule, makeWrapper, restic, openssh }:

buildGoModule {
  pname = "mashed-potato";
  version = "0.1.0-dev";

  src = lib.cleanSourceWith {
    name = "mashed-potato-src";
    src = ../.;
    # Drop the dev-built binary (a root-level file, same name as the cmd/ dir) and
    # any nix `result` symlinks from the source.
    filter = path: type:
      let b = baseNameOf path;
      in !((b == "mashed-potatod" && type != "directory")
            || b == "result"
            || lib.hasPrefix "result-" b)
         && lib.cleanSourceFilter path type;
  };

  # Hash of the module set (independent of nixpkgs). Run `nix build` and paste the
  # "got:" hash if go.mod/go.sum change.
  vendorHash = "sha256-TKrgB05dpXZbr37Gt9R8joGSqCp36692sRY5ldMZhc0=";

  subPackages = [ "cmd/mashed-potatod" ];
  ldflags = [ "-s" "-w" ];

  # The binary shells out to restic (and ssh for the sftp backend); put them on
  # its PATH so it works with no extra setup.
  nativeBuildInputs = [ makeWrapper ];
  postInstall = ''
    wrapProgram $out/bin/mashed-potatod \
      --prefix PATH : ${lib.makeBinPath [ restic openssh ]}
  '';

  meta = {
    description = "Scheduled restic backups with a web UI and tray icon";
    mainProgram = "mashed-potatod";
    platforms = lib.platforms.linux;
  };
}
