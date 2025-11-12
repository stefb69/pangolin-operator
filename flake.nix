{
  description = "Pangolin Kubernetes Operator Development Environment";

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
        isDarwin = pkgs.stdenv.isDarwin;
        isAppleSilicon = isDarwin && (pkgs.stdenv.hostPlatform.system == "aarch64-darwin");
        darwinExtras = pkgs.lib.optionals isAppleSilicon [
          pkgs.colima # Docker-compatible daemon on macOS
          pkgs.docker # Docker CLI (client)
        ];
      in
      {
        devShells.default = pkgs.mkShell {
          buildInputs =
            with pkgs;
            [
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
            ]
            ++ darwinExtras;

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
            echo "Development workflow:"
            echo "  nix run .#setup-cluster          # Create local kind cluster"
            echo "  nix run .#deploy-operator        # Build and deploy operator"
            echo "  nix run .#cleanup                # Clean up everything"
            echo ""
            echo "Release commands:"
            echo "  nix run .#release-docker-dev               # Push dev build"
            echo "  nix run .#release-multiarch-dev            # Push dev build (multi-arch)"
            echo "  nix run .#release-docker [v0.1.0]          # Push version"
            echo "  nix run .#release-multiarch [v0.1.0]       # Push version (multi-arch)"
            echo ""
            echo "Quick start:"
            echo "  make build                       # Build operator binary"
            echo "  make test                        # Run tests"
            echo "  make manifests                   # Generate CRDs and RBAC"
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

            # Apple Silicon specifics
            if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ]; then
              alias osdk-init='operator-sdk init --plugins=go/v4'
              if command -v colima >/dev/null 2>&1; then
                if colima status default >/dev/null 2>&1; then
                  export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
                  echo "Using Colima Docker socket at $DOCKER_HOST"
                else
                  echo "Colima not running."
                  echo "  Start it with: colima start --cpu 4 --memory 6 --arch aarch64"
                  echo '  export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"'
                fi
              fi
            fi
          '';
        };

        apps = {
          # Create and setup a local kind cluster
          setup-cluster = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "setup-cluster" ''
              set -e
              echo "🔧 Setting up local Kubernetes cluster..."
              CLUSTER_NAME="pangolin-dev"

              # On macOS ARM, prefer colima
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

              # On macOS ARM, prefer colima
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

          # Push dev build to Docker Hub (single arch)
          release-docker-dev = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "release-docker-dev" ''
              set -e

              # Generate dev tag with git commit hash and timestamp
              GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
              TIMESTAMP=$(date +%Y%m%d-%H%M%S)
              VERSION="dev-$TIMESTAMP-$GIT_COMMIT"

              REGISTRY="docker.io"
              IMAGE="bovfbovf/pangolin-operator"
              FULL_IMAGE="$REGISTRY/$IMAGE:$VERSION"
              DEV_TAG="$REGISTRY/$IMAGE:dev"

              echo "🐳 Building and pushing dev build"
              echo "   Version: $VERSION"
              echo "   Image: $FULL_IMAGE"

              # On macOS ARM, prefer colima
              if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ] && command -v colima >/dev/null 2>&1; then
                colima status default >/dev/null 2>&1 || colima start --cpu 4 --memory 6 --arch aarch64
                export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
                echo "ℹ️  Using Colima Docker socket at $DOCKER_HOST"
              fi

              # Check if logged in to Docker Hub
              if ! docker info 2>/dev/null | grep -q "Username:"; then
                echo "⚠️  Not logged in to Docker Hub"
                echo "   Run: docker login"
                exit 1
              fi

              echo "🔨 Building operator..."
              make build

              echo "📦 Building Docker image..."
              make docker-build IMG="$FULL_IMAGE"

              # Tag as dev
              echo "🏷️  Tagging as dev..."
              docker tag "$FULL_IMAGE" "$DEV_TAG"

              echo "⬆️  Pushing to Docker Hub..."
              docker push "$FULL_IMAGE"
              docker push "$DEV_TAG"

              echo ""
              echo "✅ Successfully pushed dev build!"
              echo "   Versioned: $FULL_IMAGE"
              echo "   Dev tag:   $DEV_TAG"
              echo ""
              echo "📝 Git status:"
              git status --short 2>/dev/null || echo "   Not a git repository"
            '';
          };

          # Push dev build to Docker Hub (multi-arch)
          release-multiarch-dev = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "release-multiarch-dev" ''
              set -e

              GIT_COMMIT=$(git rev-parse --short HEAD 2>/dev/null || echo "unknown")
              TIMESTAMP=$(date +%Y%m%d-%H%M%S)
              VERSION="dev-$TIMESTAMP-$GIT_COMMIT"

              REGISTRY="docker.io"
              IMAGE="bovfbovf/pangolin-operator"
              FULL_IMAGE="$REGISTRY/$IMAGE:$VERSION"
              DEV_TAG="$REGISTRY/$IMAGE:dev"

              echo "🐳 Building multi-arch dev build: $FULL_IMAGE"
              echo "   Method: Cross-compile locally, then build images"

              # Colima setup
              if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ] && command -v colima >/dev/null 2>&1; then
                if ! colima status default >/dev/null 2>&1; then
                  colima start --cpu 6 --memory 8 --arch aarch64 --vm-type vz
                fi
                export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
              fi

              # Check Docker login
              if ! docker info 2>/dev/null | grep -q "Username:"; then
                echo "⚠️  Not logged in to Docker Hub"
                echo "   Run: docker login"
                exit 1
              fi

              # Build binaries locally (avoids buildx Go compiler segfault)
              echo "🔨 Cross-compiling binaries locally..."
              mkdir -p bin

              echo "   Building for linux/amd64..."
              CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
                -ldflags="-w -s" \
                -o bin/manager-amd64 \
                cmd/main.go

              echo "   Building for linux/arm64..."
              CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
                -ldflags="-w -s" \
                -o bin/manager-arm64 \
                cmd/main.go

              # Create multiarch Dockerfile
              cat > Dockerfile.multiarch <<'EOF'
              FROM gcr.io/distroless/static:nonroot
              WORKDIR /
              ARG TARGETARCH
              COPY bin/manager-''${TARGETARCH} /manager
              USER 65532:65532
              ENTRYPOINT ["/manager"]
              EOF

              # Setup buildx
              if ! docker buildx ls | grep -q "multiarch"; then
                echo "🔧 Creating buildx builder..."
                docker buildx create --name multiarch \
                  --driver docker-container \
                  --use
              else
                docker buildx use multiarch
              fi

              # Build multi-arch images (just packaging pre-built binaries)
              echo "📦 Building multi-arch images..."
              docker buildx build \
                --platform linux/amd64,linux/arm64 \
                --tag "$FULL_IMAGE" \
                --tag "$DEV_TAG" \
                --file Dockerfile.multiarch \
                --push \
                .

              # Cleanup
              rm -f bin/manager-amd64 bin/manager-arm64 Dockerfile.multiarch

              echo ""
              echo "✅ Multi-arch dev build complete!"
              echo "   Platforms: linux/amd64, linux/arm64"
              echo "   Versioned: $FULL_IMAGE"
              echo "   Dev tag:   $DEV_TAG"
            '';
          };

          # Release and push to Docker Hub (single arch)
          release-docker = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "release-docker" ''
              set -e

              VERSION="''${1:-latest}"
              REGISTRY="docker.io"
              IMAGE="bovfbovf/pangolin-operator"
              FULL_IMAGE="$REGISTRY/$IMAGE:$VERSION"

              echo "🐳 Building and pushing $FULL_IMAGE"

              # On macOS ARM, prefer colima
              if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ] && command -v colima >/dev/null 2>&1; then
                colima status default >/dev/null 2>&1 || colima start --cpu 4 --memory 6 --arch aarch64
                export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
                echo "ℹ️  Using Colima Docker socket at $DOCKER_HOST"
              fi

              # Check if logged in to Docker Hub
              if ! docker info 2>/dev/null | grep -q "Username:"; then
                echo "⚠️  Not logged in to Docker Hub"
                echo "   Run: docker login"
                exit 1
              fi

              echo "🔨 Building operator..."
              make build

              echo "📦 Building Docker image..."
              make docker-build IMG="$FULL_IMAGE"

              # Also tag as latest if this is a version tag
              if [ "$VERSION" != "latest" ]; then
                echo "🏷️  Tagging as latest..."
                docker tag "$FULL_IMAGE" "$REGISTRY/$IMAGE:latest"
              fi

              echo "⬆️  Pushing to Docker Hub..."
              make docker-push IMG="$FULL_IMAGE"

              if [ "$VERSION" != "latest" ]; then
                echo "⬆️  Pushing latest tag..."
                docker push "$REGISTRY/$IMAGE:latest"
              fi

              echo ""
              echo "✅ Successfully pushed!"
              echo "   Image: $FULL_IMAGE"
              if [ "$VERSION" != "latest" ]; then
                echo "   Also: $REGISTRY/$IMAGE:latest"
              fi
              echo ""
              echo "🚀 Update dist/install.yaml with:"
              echo "   make build-installer IMG=$FULL_IMAGE"
            '';
          };

          # Build multi-arch images for releases
          release-multiarch = flake-utils.lib.mkApp {
            drv = pkgs.writeShellScriptBin "release-multiarch" ''
              set -e

              VERSION="''${1:-latest}"
              REGISTRY="docker.io"
              IMAGE="bovfbovf/pangolin-operator"
              FULL_IMAGE="$REGISTRY/$IMAGE:$VERSION"

              echo "🐳 Building multi-arch release: $FULL_IMAGE"
              echo "   Method: Cross-compile locally, then build images"

              # On macOS ARM, prefer colima
              if [ "$(uname -s)" = "Darwin" ] && [ "$(uname -m)" = "arm64" ] && command -v colima >/dev/null 2>&1; then
                if ! colima status default >/dev/null 2>&1; then
                  colima start --cpu 6 --memory 8 --arch aarch64 --vm-type vz
                fi
                export DOCKER_HOST="unix://$HOME/.colima/default/docker.sock"
              fi

              # Check Docker login
              if ! docker info 2>/dev/null | grep -q "Username:"; then
                echo "⚠️  Not logged in to Docker Hub"
                echo "   Run: docker login"
                exit 1
              fi

              # Build binaries locally (avoids buildx Go compiler segfault)
              echo "🔨 Cross-compiling binaries locally..."
              mkdir -p bin

              echo "   Building for linux/amd64..."
              CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
                -ldflags="-w -s" \
                -o bin/manager-amd64 \
                cmd/main.go

              echo "   Building for linux/arm64..."
              CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build \
                -ldflags="-w -s" \
                -o bin/manager-arm64 \
                cmd/main.go

              # Create multiarch Dockerfile
              cat > Dockerfile.multiarch <<'EOF'
              FROM gcr.io/distroless/static:nonroot
              WORKDIR /
              ARG TARGETARCH
              COPY bin/manager-''${TARGETARCH} /manager
              USER 65532:65532
              ENTRYPOINT ["/manager"]
              EOF

              # Setup buildx
              if ! docker buildx ls | grep -q "multiarch"; then
                echo "🔧 Creating buildx builder..."
                docker buildx create --name multiarch \
                  --driver docker-container \
                  --use
              else
                docker buildx use multiarch
              fi

              # Build multi-arch images
              echo "📦 Building multi-arch images..."
              docker buildx build \
                --platform linux/amd64,linux/arm64 \
                --tag "$FULL_IMAGE" \
                --file Dockerfile.multiarch \
                --push \
                .

              if [ "$VERSION" != "latest" ]; then
                echo "🏷️  Also pushing as latest..."
                docker buildx build \
                  --platform linux/amd64,linux/arm64 \
                  --tag "$REGISTRY/$IMAGE:latest" \
                  --file Dockerfile.multiarch \
                  --push \
                  .
              fi

              # Cleanup
              rm -f bin/manager-amd64 bin/manager-arm64 Dockerfile.multiarch

              echo ""
              echo "✅ Multi-arch release complete!"
              echo "   Platforms: linux/amd64, linux/arm64"
              echo "   Image: $FULL_IMAGE"
              if [ "$VERSION" != "latest" ]; then
                echo "   Also: $REGISTRY/$IMAGE:latest"
              fi
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
              rm -f Dockerfile.multiarch
              rm -f bin/manager-amd64 bin/manager-arm64

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
      }
    );
}
