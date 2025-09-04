{
  description = "Pangolin Kubernetes Operator Development Environment";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachDefaultSystem (system:
      let
        pkgs = nixpkgs.legacyPackages.${system};
        isDarwin = pkgs.stdenv.isDarwin;
        isAppleSilicon = isDarwin && (pkgs.stdenv.hostPlatform.system == "aarch64-darwin");
        darwinExtras = pkgs.lib.optionals isAppleSilicon [
          pkgs.colima         # Docker-compatible daemon on macOS
          pkgs.docker         # Docker CLI (client)
        ];
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs = with pkgs; [
            # Go development
            go
            gopls
            go-tools
            gofumpt
            golangci-lint
            gotest
            gotestsum

            # Kubernetes tools
            kubectl
            kubernetes-helm
            kind
            kustomize
            operator-sdk
            kubebuilder

            # Container tools
            docker

            # General development tools
            git
            gnumake
            which
            curl
            jq
            yq-go

            # Useful utilities
            tree
            bat
            fd
            ripgrep
          ] ++ darwinExtras;

          shellHook = ''
            echo "🦎 Pangolin Operator Development Environment"
            echo "============================================="
            echo "Tools available:"
            echo "  • Go $(go version | cut -d' ' -f3)"
            echo "  • kubectl $(kubectl version --client --short 2>/dev/null | cut -d' ' -f3 || echo 'latest')"
            echo "  • operator-sdk $(operator-sdk version 2>/dev/null | head -1 | cut -d'"' -f2 || echo 'latest')"
            echo "  • kind $(kind version 2>/dev/null || echo 'latest')"
            echo "  • Docker $(docker --version 2>/dev/null | cut -d' ' -f3 | tr -d ',' || echo 'not available')"
            echo ""
            echo "Quick start:"
            echo "  operator-sdk init --domain pangolin.io --repo github.com/yourusername/pangolin-operator"
            echo "  operator-sdk create api --group tunnel --version v1alpha1 --kind PangolinTunnel --resource --controller"
            echo ""
            echo "Development cluster:"
            echo "  kind create cluster --name pangolin-dev"
            echo "  kind export kubeconfig --name pangolin-dev --kubeconfig ./.kubeconfig"
            echo "  export KUBECONFIG=\$PWD/.kubeconfig"
            echo ""

            # Set up Go environment
            export GOPROXY="https://proxy.golang.org,direct"
            export GOSUMDB="sum.golang.org"
            export CGO_ENABLED="0"

            # Kubernetes development
            export KUBECONFIG="$PWD/.kubeconfig"

            # Create local directories
            mkdir -p ./bin
            export PATH="$PWD/bin:$PATH"

            # Git configuration for operator development
            if [ -d .git ]; then
              git config --local core.hooksPath .githooks 2>/dev/null || true
            fi

            # Apple Silicon specifics: prefer go/v4 plugin; DO NOT start colima here (keep direnv non-blocking)
            if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ]; then
              alias osdk-init='operator-sdk init --plugins=go/v4'  # recommended scaffold for darwin/arm64
              if command -v colima >/dev/null 2>&1; then
                # Only wire Docker if Colima is already running (non-blocking)
                if colima status default >/dev/null 2>&1; then
                  export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
                  echo "Using Colima Docker socket at $DOCKER_HOST"
                else
                  echo "Colima not running."
                  echo "  Start it with: colima start --cpu 4 --memory 6 --arch aarch64"
                  echo '  export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"'
                  echo "  Alternatively: docker context use colima"
                fi
              fi
            fi
          '';
        };

        # Apps for common development tasks
        apps = {
          # Create and setup a local kind cluster
          setup-cluster = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "setup-cluster" ''
              set -e
              echo "🔧 Setting up local Kubernetes cluster..."
              CLUSTER_NAME="pangolin-dev"

              # On macOS ARM, prefer colima as Docker daemon and export DOCKER_HOST for this session
              if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ] && command -v colima >/dev/null 2>&1; then
                colima status default >/dev/null 2>&1 || colima start --cpu 4 --memory 6 --arch aarch64
                export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
                echo "ℹ️  Using Colima Docker socket at $DOCKER_HOST"
              fi

              # Check if cluster already exists
              if kind get clusters 2>/dev/null | grep -q "$CLUSTER_NAME"; then
                echo "📋 Cluster $CLUSTER_NAME already exists"
                read -p "Delete and recreate? (y/N): " -n 1 -r
                echo
                if [[ $REPLY =~ ^[Yy]$ ]]; then
                  kind delete cluster --name "$CLUSTER_NAME"
                else
                  echo "Using existing cluster"
                  kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig ./.kubeconfig
                  export KUBECONFIG="$PWD/.kubeconfig"
                  kubectl cluster-info
                  exit 0
                fi
              fi

              # Create kind cluster
              cat <<EOF | kind create cluster --name "$CLUSTER_NAME" --config=-
              kind: Cluster
              apiVersion: kind.x-k8s.io/v1alpha4
              nodes:
              - role: control-plane
                kubeadmConfigPatches:
                - |
                  kind: InitConfiguration
                  nodeRegistration:
                    kubeletExtraArgs:
                      node-labels: "ingress-ready=true"
                extraPortMappings:
                - containerPort: 80
                  hostPort: 8080
                  protocol: TCP
                - containerPort: 443
                  hostPort: 8443
                  protocol: TCP
              - role: worker
              EOF

              # Export kubeconfig
              kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig ./.kubeconfig
              export KUBECONFIG="$PWD/.kubeconfig"

              echo ""
              echo "✅ Cluster ready!"
              echo "   Export kubeconfig: export KUBECONFIG=\$PWD/.kubeconfig"
              echo "   Cluster info:"
              kubectl cluster-info
              echo ""
              echo "   Nodes:"
              kubectl get nodes -o wide
            '';
          };

          # Build and deploy the operator locally
          deploy-operator = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "deploy-operator" ''
              set -e
              if [ ! -f ".kubeconfig" ]; then
                echo "❌ No local kubeconfig found. Run 'nix run .#setup-cluster' first."
                exit 1
              fi
              export KUBECONFIG="$PWD/.kubeconfig"

              # On macOS ARM, prefer colima as Docker daemon and export DOCKER_HOST
              if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ] && command -v colima >/dev/null 2>&1; then
                colima status default >/dev/null 2>&1 || colima start --cpu 4 --memory 6 --arch aarch64
                export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
                echo "ℹ️  Using Colima Docker socket at $DOCKER_HOST"
              fi

              echo "🔨 Building operator..."
              make build

              echo "🐳 Building Docker image..."
              IMG="pangolin-operator:dev"
              make docker-build IMG="$IMG"

              echo "📦 Loading image into kind cluster..."
              kind load docker-image "$IMG" --name pangolin-dev

              echo "🚀 Deploying operator..."
              make deploy IMG="$IMG"

              echo ""
              echo "✅ Operator deployed!"
              echo "   Check status:"
              echo "   kubectl -n pangolin-operator-system get pods"
              echo "   kubectl -n pangolin-operator-system logs deployment/pangolin-operator-controller-manager -c manager"
            '';
          };

          # Clean up everything
          cleanup = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "cleanup" ''
              set -e
              echo "🧹 Cleaning up development environment..."

              # Delete kind cluster
              if kind get clusters 2>/dev/null | grep -q "pangolin-dev"; then
                echo "🗑️  Deleting kind cluster..."
                kind delete cluster --name pangolin-dev
              fi

              # Clean up local files
              rm -f .kubeconfig

              # Clean up Docker images
              if command -v docker &> /dev/null; then
                echo "🐳 Cleaning up Docker images..."
                docker image rm pangolin-operator:dev 2>/dev/null || true
                docker system prune -f
              fi

              echo "✅ Cleanup complete!"
            '';
          };
        };

        # For building releases
        packages.default = pkgs.buildGoModule rec {
          pname = "pangolin-operator";
          version = "0.1.0";

          src = ./.;

          # You'll need to update this after running `go mod tidy`
          vendorHash = null;

          subPackages = [ "main.go" ];

          buildInputs = with pkgs; [ git ];

          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];

          meta = with pkgs.lib; {
            description = "Kubernetes operator for Pangolin tunnel management";
            license = licenses.asl20;
          };
        };
      });
}
