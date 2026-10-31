{
  description = "Kopoka - A Telegram bot for handling Pixiv and X (Twitter) links";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs =
    {
      self,
      nixpkgs,
      flake-utils,
    }:
    flake-utils.lib.eachDefaultSystem (
      system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
      in
      {
        packages.default = pkgs.buildGoModule {
          pname = "kopoka";
          version = "0.1.0";
          src = ./.;
          vendorHash = "sha256-LUs/MMEzg/TWb5sQOo9XCYJqr59dwyqoEODsZOAlNiM=";
          subPackages = [ "src" ];
          ldflags = [
            "-s"
            "-w"
          ];
          postInstall = ''
            mv $out/bin/src $out/bin/kopoka
          '';
          meta = with pkgs.lib; {
            description = "A Telegram bot for handling Pixiv and X (Twitter) links";
            homepage = "https://github.com/sleroq/kopoka";
            license = licenses.gpl3;
            maintainers = [ maintainers.sleroq or "sleroq" ];
            platforms = platforms.linux ++ platforms.darwin;
          };
        };

        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            go
            gopls
            gotools
            go-tools
          ];
          shellHook = ''
            echo "Kopoka development environment"
            echo "Go version: $(go version)"
            echo ""
            echo "Available commands:"
            echo "  go build -o dist/kopoka ./src     # Build the bot"
            echo "  go run ./src                      # Run the bot directly"
            echo "  go mod tidy                      # Update dependencies"
            echo ""
            echo "Make sure to set the required environment variables:"
            echo "  BOT_TOKEN, PIXIV_ACCESS_TOKEN, PIXIV_REFRESH_TOKEN, PIXIV_TOKEN_DEADLINE"
          '';
        };

        apps.default = {
          type = "app";
          program = "${self.packages.${system}.default}/bin/kopoka";
        };
      }
    );
}
