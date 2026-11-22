# The community provider image, bringyour/community-provider: the released
# provider (the sn miner) for linux/amd64 and linux/arm64, for operators who
# run it in Docker. It packages the release's own binaries; it never builds
# the provider:
#
#   PROVIDER_BINARIES=<directory with linux/amd64/provider and linux/arm64/provider> \
#       warpctl build community all/community/provider/Makefile
#   all/community/provider/promote.sh <version>
#
# PROVIDER_BINARIES is the sn cli/miner build directory of a release run, or
# an unpacked urnetwork-provider-<version>.tar.gz. warpctl names the image
# after this directory and runs govulncheck on the binaries before
# warp_build_image publishes the version tag; promote.sh then points the tags
# operators follow (latest, beta-latest, g1-latest to g4-latest) at it.

.PHONY: all clean build warp_build warp_build_image test

all: clean build

clean:
	rm -rf build

# Each binary must be the provider of this version (main.Version is stamped
# into it), so a version tag never names another build.
build:
	@test -n "$(PROVIDER_BINARIES)" || { echo "PROVIDER_BINARIES must name the release's provider binaries" >&2; exit 1; }
	@test -n "$(WARP_VERSION)" || { echo "WARP_VERSION must be set (warpctl build sets it)" >&2; exit 1; }
	@for arch in amd64 arm64; do \
		mkdir -p build/linux/$$arch && \
		install -m 755 "$(PROVIDER_BINARIES)/linux/$$arch/provider" build/linux/$$arch/provider && \
		LC_ALL=C grep -a -q -F -- "$(WARP_VERSION)" build/linux/$$arch/provider || \
		{ echo "$(PROVIDER_BINARIES)/linux/$$arch/provider is not provider $(WARP_VERSION)" >&2; exit 1; }; \
	done

warp_build:
	@echo "warp_build must be run through warpctl build so govulncheck gates image publication." >&2
	@false

warp_build_image:
	docker buildx build --progress=plain \
		--build-arg warp_env=${WARP_ENV} \
		--build-arg warp_version=${WARP_VERSION} \
		--platform linux/arm64/v8,linux/amd64 \
		-t ${WARP_DOCKER_NAMESPACE}/${WARP_DOCKER_IMAGE}:${WARP_DOCKER_VERSION} \
		--no-cache \
		--push \
		.

# The wrapper's tests, as root in the image's base.
test:
	docker run --rm -v "$(CURDIR):/t:ro" ubuntu:22.04 sh /t/bringyour-provider.test.sh
