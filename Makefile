PLUGIN_NAME ?= jakubs22/juicefs
# Target arch for single-arch builds (amd64|arm64)
ARCH ?= $(shell uname -m | sed 's/aarch64/arm64/;s/x86_64/amd64/')
# Default tag follows arch (e.g., amd64-latest / arm64-latest)
PLUGIN_TAG ?= $(ARCH)-latest
PLATFORMS ?= linux/amd64,linux/arm64
BUILDER_NAME ?= juicefs-builder
DOCKER_CONTEXT ?= $(shell docker context show 2>/dev/null || echo default)
rootfs: JUICEFS_CE_VERSION ?= $(shell curl -s https://api.github.com/repos/juicedata/juicefs/releases/latest | grep 'tag_name' | cut -d '"' -f 4 | tr -d 'v')

all: clean rootfs create

all-buildx: clean rootfs-buildx create

## -------- Docker image (for Hub: https://hub.docker.com/r/$(PLUGIN_NAME)) --------
## Build and push a multi-arch image tagged with "latest" and the JuiceFS CE version.
## Usage:
##   DOCKER_USERNAME=<user> DOCKER_PASSWORD=<pass> make image-push
## Optionally override platforms:
##   PLATFORMS=linux/amd64,linux/arm64 make image-push
image-login:
	@echo "### docker login to Docker Hub"
	@echo "$$DOCKER_PASSWORD" | docker login -u "$$DOCKER_USERNAME" --password-stdin

image-push: JUICEFS_CE_VERSION ?= $(shell curl -s https://api.github.com/repos/juicedata/juicefs/releases/latest | grep 'tag_name' | cut -d '"' -f 4 | tr -d 'v')
image-push:
	@echo "### setup buildx builder"
	@docker buildx create --name ${BUILDER_NAME} --platform ${PLATFORMS} --use || docker buildx use ${BUILDER_NAME}
	@echo "### docker buildx: build and push multi-arch image for ${PLATFORMS}"
	@docker buildx build --platform ${PLATFORMS} \
		--build-arg="JUICEFS_CE_VERSION=${JUICEFS_CE_VERSION}" \
		-t ${PLUGIN_NAME}:latest \
		-t ${PLUGIN_NAME}:${JUICEFS_CE_VERSION} \
		--push .

## -------------------------------------------------------------------------------

rootfs-buildx-arch: ## Build rootfs image for a single architecture and load locally
		@echo "### setup buildx builder"
		@docker buildx create --name ${BUILDER_NAME} --platform linux/${ARCH} --use || docker buildx use ${BUILDER_NAME}
		@echo "### docker buildx: rootfs image for linux/${ARCH}"
		@docker buildx build --platform linux/${ARCH} --build-arg="JUICEFS_CE_VERSION=${JUICEFS_CE_VERSION}" --build-arg="JUICEFS_EE_URL=${JUICEFS_EE_URL}" -t ${PLUGIN_NAME}:rootfs --load .
		@echo "### create rootfs directory in ./plugin/rootfs"
		@mkdir -p ./plugin/rootfs
		@docker rm -vf tmp >/dev/null 2>&1 || true
		@docker create --name tmp ${PLUGIN_NAME}:rootfs >/dev/null
		@docker export tmp | tar -x -C ./plugin/rootfs
	@echo "### copy config.json to ./plugin/"
	@cp config.json ./plugin/
	@docker rm -vf tmp >/dev/null

clean:
	@echo "### rm ./plugin"
	@rm -rf ./plugin

rootfs:
		@echo "### docker build: rootfs image with docker-volume-juicefs"
		@docker build --build-arg="JUICEFS_CE_VERSION=${JUICEFS_CE_VERSION}" --build-arg="JUICEFS_EE_URL=${JUICEFS_EE_URL}" -t ${PLUGIN_NAME}:rootfs .
		@echo "### create rootfs directory in ./plugin/rootfs"
		@mkdir -p ./plugin/rootfs
		@docker create --name tmp ${PLUGIN_NAME}:rootfs
		@docker export tmp | tar -x -C ./plugin/rootfs
		@echo "### copy config.json to ./plugin/"
		@cp config.json ./plugin/
		@docker rm -vf tmp

rootfs-buildx:
		@echo "### setup buildx builder"
		@docker buildx create --name ${BUILDER_NAME} --platform ${PLATFORMS} --use || docker buildx use ${BUILDER_NAME}
		@echo "### docker buildx: rootfs image with docker-volume-juicefs for ${PLATFORMS}"
		@docker buildx build --platform ${PLATFORMS} --build-arg="JUICEFS_CE_VERSION=${JUICEFS_CE_VERSION}" --build-arg="JUICEFS_EE_URL=${JUICEFS_EE_URL}" -t ${PLUGIN_NAME}:rootfs --load .
		@echo "### create rootfs directory in ./plugin/rootfs"
		@mkdir -p ./plugin/rootfs
		@docker create --name tmp ${PLUGIN_NAME}:rootfs
		@docker export tmp | tar -x -C ./plugin/rootfs
		@echo "### copy config.json to ./plugin/"
	@cp config.json ./plugin/
	@docker rm -vf tmp

create:
	@echo "### remove existing plugin ${PLUGIN_NAME}:${PLUGIN_TAG} if exists"
	@docker --context ${DOCKER_CONTEXT} plugin rm -f ${PLUGIN_NAME}:${PLUGIN_TAG} || true
	@echo "### create new plugin ${PLUGIN_NAME}:${PLUGIN_TAG} from ./plugin"
	@docker --context ${DOCKER_CONTEXT} plugin create ${PLUGIN_NAME}:${PLUGIN_TAG} ./plugin

enable:
	@echo "### enable plugin ${PLUGIN_NAME}:${PLUGIN_TAG}"
	@docker --context ${DOCKER_CONTEXT} plugin disable -f ${PLUGIN_NAME}:${PLUGIN_TAG} 2>/dev/null || true
	docker --context ${DOCKER_CONTEXT} plugin enable ${PLUGIN_NAME}:${PLUGIN_TAG}

disable:
	@echo "### disable plugin ${PLUGIN_NAME}:${PLUGIN_TAG}"
	@docker --context ${DOCKER_CONTEXT} plugin disable ${PLUGIN_NAME}:${PLUGIN_TAG} 2>/dev/null || \
	  echo "### warning: plugin ${PLUGIN_NAME}:${PLUGIN_TAG} is still in use; leaving enabled for diagnostics"

test: enable volume compose disable

volume:
	@echo "### test volume create and mount"
	docker volume create -d ${PLUGIN_NAME}:${PLUGIN_TAG} -o name=${JFS_VOL} -o token=${JFS_TOKEN} -o accesskey=${JFS_ACCESSKEY} -o secretkey=${JFS_SECRETKEY} -o subdir=${JFS_SUBDIR} jfsvolume

	docker run --rm -v jfsvolume:/write busybox sh -c "echo hello > /write/world"
	docker run --rm -v jfsvolume:/read busybox sh -c "grep -Fxq hello /read/world"
	docker run --rm -v jfsvolume:/list busybox sh -c "ls /list"

	docker volume rm jfsvolume

compose:
	@echo "### test compose"
	docker-compose -f docker-compose.yml up
	docker-compose -f docker-compose.yml down --volume

push:
	@echo "### push plugin ${PLUGIN_NAME}:${PLUGIN_TAG}"
	docker plugin push ${PLUGIN_NAME}:${PLUGIN_TAG}

clean-buildx:
	@echo "### remove buildx builder"
	@docker buildx rm ${BUILDER_NAME} || true

# Create and enable canonical plugin name on target context: juicefs-volume:latest
create-canonical:
	@echo "### create canonical plugin juicefs-volume:latest on context ${DOCKER_CONTEXT}"
	@docker --context ${DOCKER_CONTEXT} plugin disable -f juicefs-volume:latest 2>/dev/null || true
	@docker --context ${DOCKER_CONTEXT} plugin rm -f juicefs-volume:latest 2>/dev/null || true
	@docker --context ${DOCKER_CONTEXT} plugin create juicefs-volume:latest ./plugin
	@docker --context ${DOCKER_CONTEXT} plugin enable juicefs-volume:latest
