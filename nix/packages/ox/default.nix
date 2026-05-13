{
  perSystem = {
    config,
    inputs',
    lib,
    pkgs,
    ...
  }: {
    devshells.default = {
      packages = with pkgs; [dolt git-lfs];
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
        (readonly (noescape "~/.config/sageox"))
        (readwrite (noescape "~/.local/share/sageox"))
      ];

    packages = {
      default = config.packages.ox;
      ox = pkgs.buildGoModule (finalAttrs: {
        pname = "ox";
        version = "0.8.1"; # internal/version/version.go
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
        vendorHash = "sha256-ET+Y4YKgNCMnCNJjcqN8hb0EjE62XJBavhqgfNeqX0Y=";
        # FIXME: TestExtractGitHubFacts* requires `git` on PATH
        doCheck = false;
      });
    };
  };
}
