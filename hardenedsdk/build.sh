#!/bin/bash

error_trap () {
    code=$?
    if [ $code != 0 ]; then
        echo "error($code): $1"
        exit $code
    fi
}

(apt-get update &&
	apt-get install -y build-essential openjdk-21-jdk-headless curl unzip &&
	update-alternatives --auto java &&
	curl -L https://go.dev/dl/go1.24.2.linux-amd64.tar.gz | tar -xz -C /usr/local/)
error_trap 'root init'

(mkdir -p /opt/android-sdk/cmdline-tools &&
	unzip /urnetwork/build/hardenedsdk/commandlinetools-linux-13114758_latest.zip -d /opt/android-sdk/cmdline-tools &&
	mv /opt/android-sdk/cmdline-tools/cmdline-tools /opt/android-sdk/cmdline-tools/latest)
error_trap 'android sdk init'

export PATH="$PATH:/opt/android-sdk/cmdline-tools/latest/bin"
export ANDROID_HOME=/opt/android-sdk

ANDROID_NDK_VERSION=28.0.13004108
echo yes | sdkmanager "ndk;$ANDROID_NDK_VERSION"
error_trap 'android ndk install'
export ANDROID_NDK_HOME="$ANDROID_HOME/ndk/$ANDROID_NDK_VERSION"

(keytool -genkey -v \
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
elif [[ "$go_version" =~ "go version go1.24.2" ]]; then
	echo "go check: go will use system go ($go_version)"
else
	echo "go check: system go must either be 1.24.2 or not installed"
	exit 1
fi
java_version=`java -version 2>&1`
if [[ ! "$java_version" =~ 'openjdk version "21.0.6"' ]]; then
    echo "java check: 21.0.6 required ($java_version)"
    exit 1
fi

export WARP_HOME=/urnetwork
export BRINGYOUR_HOME=/urnetwork/build

(cd $BRINGYOUR_HOME/android/app/ &&
	./gradlew clean buildSdk assemblePlay)
