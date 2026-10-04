# Catenary — the Flutter client

The Android and Linux desktop client (E5). This directory is the app: widgets, themes and platform seams. The protocol half — journal, transport, credential layer, outbox — is `../dart-client` (`catenary_client`), which imports no Flutter, over the generated wire package in `../dart`.

```sh
. ~/dev-tools/env.sh      # Flutter 3.44.2, Temurin 17, the Android SDK
flutter pub get
flutter analyze && flutter test
flutter run               # a device on adb, or `-d linux` once CANT-45 lands
```

`lib/tokens.dart` and `lib/metrics.dart` are the web client's `web/src/styles/tokens.css` in a second runtime. The names are the contract; `test/tokens_test.dart` reads the CSS and fails when either side moves without the other.
