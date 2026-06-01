{
  perSystem = {
    config,
    inputs',
    lib,
    pkgs,
    ...
  }: {
    devshells.default = {
      packages = with pkgs; [dolt gcc git-lfs gnumake golangci-lint gotools];
      packagesFrom = [config.packages.ox];
    };

    jail.additionalCombinators = cs:
      with cs; [
        (add-pkg-deps [
          config.packages.claude-code-unwrapped
          config.packages.ox
          pkgs.dolt
          pkgs.git-lfs
        ])
        (readwrite (noescape "~/.config/sageox"))
        (readwrite (noescape "~/.local/share/sageox"))
        (add-runtime ''
          # NOTE: This is where the IPC socket lands. Must already exist!
          RUNTIME_ARGS+=(--bind "$XDG_RUNTIME_DIR/sageox" "$XDG_RUNTIME_DIR/sageox")
        '')
      ];

    packages = {
      default = config.packages.ox;
      ox = pkgs.buildGoModule (finalAttrs: {
        pname = "ox";
        version = "0.9.0"; # internal/version/version.go:5
        src = lib.fileset.toSource {
          root = ../../..;
          fileset = lib.fileset.unions [
            ../../../cmd
            ../../../extensions
            ../../../go.mod
            ../../../go.sum
            ../../../internal
            ../../../pkg
            ../../../security
            ../../../test
            ../../../tests
          ];
        };
        subPackages = [
          "cmd/ox"
          "cmd/ox-adapter-claude-code"
          "cmd/ox-adapter-codex"
        ];
        vendorHash = "sha256-W0SHxFIIaW4YKiej2ks9unZNnaCf4MrvIaLZWFIQ4gc=";
        # FIXME: TestExtractGitHubFacts* require `git` on PATH
        doCheck = false;
      });
    };
  };
}
