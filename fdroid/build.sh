#!/bin/bash

(sudo apt-get update &&
	sudo apt-get install -y gcc libc-dev make &&
	echo "deb https://deb.debian.org/debian trixie main" | sudo tee /etc/apt/sources.list.d/trixie.list &&
	sudo apt-get update &&
	sudo apt-get install -y -t trixie openjdk-23-jdk-headless &&
	sudo update-alternatives --auto java &&
	curl -L https://go.dev/dl/go1.24.2.linux-amd64.tar.gz | sudo tar -xz -C /usr/local/)

export ANDROID_HOME=/opt/android-sdk

sdkmanager 'ndk;28.0.13004108'
export ANDROID_NDK_HOME=$ANDROID_HOME/ndk/28.0.13004108

(mkdir $HOME/.android &&
	keytool -genkey -v \
		-keystore $HOME/.android/debug.keystore \
		-alias androiddebugkey \
		-dname "CN=ur.network, OU=URnetwork, O=URnetwork, L=San Francisco, S=California, C=US" \
		-storepass android \
		-keypass android \
		-keyalg RSA \
		-validity 14000)

go_version=`go version 2> /dev/null`
if [ "$go_version" == "" ]; then
	echo "go check: go will use /usr/local/go ($(/usr/local/go/bin/go version))"
elif [[ "$go_version" =~ "go version go1.24.2" ]]; then
	echo "go check: go will use system go ($go_version)"
else
	echo "go check: system go must either be 1.24.2 or not installed"
	exit 1
fi

export WARP_HOME=/urnetwork
export BRINGYOUR_HOME=/urnetwork/build

(cd $BRINGYOUR_HOME/android/app/ &&
	./gradlew clean assembleGithub)
