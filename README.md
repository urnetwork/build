# build

This repo contains the build code used for production releases. The goal is to publish reproducible builds so that anyone can build and verify the code in production.

The `main` branch of all repos is the latest production-ready code.

The project uses warp versions, `yyyy.mm.dd-version_code`, so that versions are tied to dates and not release schedules. The project does not plan to have specific release schedules in the near future, so that we can iterate more fluidly on major features. We will maintain backwards compatibility as much as possible. Anytime there is a breaking change we will document it in the corresponding repo.

`metadata/en-US/changelogs` will be populated for each version code that is published in a store.


## Build and deploy all

`all/run.sh` runs daily:
- builds all clients and services from `main`
- runs local tests
- branches and tags repos for reproducible builds
- uploads clients to test tracks
- deploys services to production over 16 hours

If you want to join the test tracks to get regular test builds via TestFlight and Google Play, please send a request to <support@ur.io>.


## Reproducible builds

Builds use the semver `<version>-<version_code>`. Each semver has a corresponding tag in each repo, `v<version>-<version_code>`. You can check out the `build` repo at this tag to get all the repos at the correct tag also.

Because warp versions are `yyyy.mm.dd`, the go modules in the version branches are translated to the `vyyyy` major by appending `/vyyyy` to the end. This makes it so that any go module can be imported as `<module>/vyyyy`.

### ABI-specific versions codes

If the version code ends in non-zero, it means you have an architecture-specific build that was done to reduce download size. In this case, just change the last digit to zero to find the git tags and branch names. 

e.g.

```
614087741 # specific to arm64
614087740 # the version code to use for git tags, branches, etc.
``` 


## Ungoogle flavor

We maintain a flavor of each version that removes all google dependencies. This is the most compatible with global devices.

Use the tag `v<version>-<version_code>-ungoogle` of the `build` repo to get the ungoogle variant.


## Example build android client

If you downloaded the app from a store, go to Apps -> URnetwork to view the version and version code for the app. You can then build your own debug copy of the app with the following.

```
# check your apk for the version and version code
# git checkout v<version>-<version_code>
git checkout v2025.4.1-58770364
git submodule update --init
cd android/app
./gradlew assembleDebug
```


## Resource notes

On macOS, the docker engine must have at least 16GiB of memory allocated.

## DockerHub Images

Production images can be found at [DockerHub](https://hub.docker.com/u/bringyour). These are deployed with the [`warpctl` tool](https://github.com/urnetwork/warp).
