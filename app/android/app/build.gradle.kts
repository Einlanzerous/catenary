plugins {
    id("com.android.application")
    // The Flutter Gradle Plugin must be applied after the Android and Kotlin Gradle plugins.
    id("dev.flutter.flutter-gradle-plugin")
}

android {
    namespace = "dev.dodson.catenary"
    compileSdk = flutter.compileSdkVersion
    ndkVersion = flutter.ndkVersion

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }

    defaultConfig {
        applicationId = "dev.dodson.catenary"
        minSdk = flutter.minSdkVersion
        targetSdk = flutter.targetSdkVersion
        versionCode = flutter.versionCode
        versionName = flutter.versionName
        manifestPlaceholders["appLabel"] = "Catenary"
    }

    buildTypes {
        // Its own application id, as the estate's other Flutter apps do: a
        // debug build installs beside a released one instead of replacing it,
        // and never shares its journal or its credential.
        debug {
            applicationIdSuffix = ".dev"
            manifestPlaceholders["appLabel"] = "Catenary dev"
        }
        release {
            // Signed with the debug key until a release lane exists; nothing
            // ships from here yet. R8 is off explicitly — AGP 9 defaults it on
            // for release, and that is its own change with its own device
            // launch (ARGY-114).
            signingConfig = signingConfigs.getByName("debug")
            isMinifyEnabled = false
            isShrinkResources = false
        }
    }
}

kotlin {
    compilerOptions {
        jvmTarget = org.jetbrains.kotlin.gradle.dsl.JvmTarget.JVM_17
    }
}

flutter {
    source = "../.."
}
