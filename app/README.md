# Catenary — the Flutter client

The Android and Linux desktop client (E5). This directory is the app: widgets, themes and platform seams. The protocol half — journal, transport, credential layer, outbox — is `../dart-client` (`catenary_client`), which imports no Flutter, over the generated wire package in `../dart`.

```sh
. ~/dev-tools/env.sh      # Flutter 3.44.2, Temurin 17, the Android SDK
flutter pub get
flutter analyze && flutter test
flutter run               # a device on adb, or `-d linux` once CANT-45 lands
```

`lib/tokens.dart` and `lib/metrics.dart` are the web client's `web/src/styles/tokens.css` in a second runtime. The names are the contract; `test/tokens_test.dart` reads the CSS and fails when either side moves without the other.

## Installing on a device

There is no release lane yet, so the phone gets a debug build by hand. The toolchain lives in `~/dev-tools` on the estate server and is not on `PATH`.

```sh
. ~/dev-tools/env.sh          # flutter 3.44.2, Temurin 17, adb
adb devices                    # the phone must be listed and unlocked
cd app
flutter pub get
flutter build apk --debug
adb install -r build/app/outputs/flutter-apk/app-debug.apk
```

- The debug build's application id is `dev.dodson.catenary.dev` (label "Catenary dev"), so it installs beside a released build and never shares its journal or credential.
- `-r` reinstalls over the existing app and keeps its data: the device stays enrolled.
- Build from a checkout at the commit you mean to ship. A server release does not update the app, and a web-only release changes nothing on the phone.
- Check the install took with `adb shell dumpsys package dev.dodson.catenary.dev | grep lastUpdateTime`.
