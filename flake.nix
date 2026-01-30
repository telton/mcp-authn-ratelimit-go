{
  description = "Flake";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-25.11";
  };

  outputs = { self, nixpkgs }:
  let
    systems = [
      "x86_64-linux"
      "aarch64-linux"
      "x86_64-darwin"
      "aarch64-darwin"
    ];

    forAllSystems = f:
      nixpkgs.lib.genAttrs systems (system:
        f (import nixpkgs { inherit system; }));
  in
  {
    devShells = forAllSystems (pkgs:
    {
      default = pkgs.mkShell {
        buildInputs = with pkgs; [
          nil
          nixd

          # Go language and tooling
          go_1_25
          gopls    # Go language server
          delve    # Go debugger
          golangci-lint
          golangci-lint-langserver

          docker
          docker-compose
        ];

        shellHook = ''
          # Set up shared Go workspace
          export GOPATH="$HOME/.cache/go"
          export GOMODCACHE="$HOME/.cache/go/pkg/mod"
          export PATH="$GOPATH/bin:$PATH"

          # Create directories if they don't exist
          mkdir -p "$GOPATH/bin" "$GOMODCACHE"

          echo "Using $(go version)"
          echo "GOPATH: $GOPATH"
          echo "GOMODCACHE: $GOMODCACHE"
        '';
      };
    });
  };
}
