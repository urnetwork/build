#!/bin/bash

error_trap () {
    code=$?
    if [ $code != 0 ]; then
        echo "error($code): $1"
        exit $code
    fi
}

(sudo apt-get update &&
	sudo apt-get install -y gcc libc-dev make &&
	echo "deb https://deb.debian.org/debian trixie main" | sudo tee /etc/apt/sources.list.d/trixie.list &&
	sudo apt-get update &&
	sudo apt-get install -y -t trixie openjdk-21-jdk-headless &&
	sudo update-alternatives --auto java &&
	curl -L https://go.dev/dl/go1.26.0.linux-amd64.tar.gz | sudo tar -xz -C /usr/local/)
error_trap 'root init'

export ANDROID_HOME=/opt/android-sdk

ANDROID_NDK_VERSION=29.0.14206865
sdkmanager "ndk;$ANDROID_NDK_VERSION" 'platforms;android-36'
error_trap 'android ndk install'
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION"

(mkdir $HOME/.android &&
	keytool -genkey -v \
		-keystore $HOME/.android/debug.keystore \
		-alias androiddebugkey \
		-dname "CN=ur.network, OU=URnetwork, O=URnetwork, L=San Francisco, S=California, C=US" \
		-storepass android \
		-keypass android \
		-keyalg RSA \
		-validity 14000)
error_trap 'android debug key init'

go_version=`go version 2> /dev/null`
if [ "$go_version" == "" ]; then
	echo "go check: go will use /usr/local/go ($(/usr/local/go/bin/go version))"
elif [[ "$go_version" =~ "go version go1.26.0" ]]; then
	echo "go check: go will use system go ($go_version)"
else
	echo "go check: system go must either be 1.26.0 or not installed"
	exit 1
fi
java_version=`java -version 2>&1`
if [[ ! "$java_version" =~ 'openjdk version "21.0."' ]]; then
    echo "java check: 21.0.x required ($java_version)"
    exit 1
fi


export WARP_HOME=/urnetwork
export BRINGYOUR_HOME=/urnetwork/build
export GRADLE_OPTS="-Dorg.gradle.daemon=false -Dorg.gradle.workers.max=1 -Dkotlin.compiler.execution.strategy=in-process"

(cd $BRINGYOUR_HOME/android/app/ &&
	./gradlew clean buildSdk assembleGithub)
