import java.io.File
import java.util.Base64
import org.jetbrains.kotlin.gradle.dsl.JvmTarget

plugins {
    id("com.android.application")
}

android {
    namespace = "com.zcc09.twinlink"
    compileSdk = 37

    defaultConfig {
        applicationId = "com.zcc09.twinlink"
        minSdk = 26
        targetSdk = 36
        versionCode = 4
        versionName = "0.1.3"
    }

    buildFeatures {
        buildConfig = true
    }

    signingConfigs {
        // A real, stable signing identity when the keystore secrets are present, so a
        // release APK can be installed over the previous one. Without them the release
        // build falls back to the debug key (still installable, different identity).
        val keystoreB64: String? = System.getenv("KEYSTORE_BASE64")
        if (!keystoreB64.isNullOrBlank()) {
            create("release") {
                val store = File(layout.buildDirectory.get().asFile, "twinlink-release.p12")
                store.parentFile?.mkdirs()
                store.writeBytes(Base64.getMimeDecoder().decode(keystoreB64))
                storeFile = store
                storeType = "pkcs12"
                storePassword = System.getenv("KEYSTORE_PASSWORD") ?: ""
                keyAlias = System.getenv("KEY_ALIAS") ?: "twinlink"
                keyPassword = System.getenv("KEY_PASSWORD") ?: System.getenv("KEYSTORE_PASSWORD") ?: ""
            }
        }
    }

    buildTypes {
        release {
            isMinifyEnabled = false
            proguardFiles(
                getDefaultProguardFile("proguard-android-optimize.txt"),
                "proguard-rules.pro"
            )
            signingConfig = signingConfigs.findByName("release")
                ?: signingConfigs.getByName("debug")
        }
    }

    testOptions {
        unitTests.isReturnDefaultValues = true
    }

    compileOptions {
        sourceCompatibility = JavaVersion.VERSION_17
        targetCompatibility = JavaVersion.VERSION_17
    }
}

kotlin {
    compilerOptions {
        jvmTarget = JvmTarget.JVM_17
    }
}

dependencies {
    implementation("androidx.core:core-ktx:1.13.1")
    implementation("androidx.appcompat:appcompat:1.6.1")
    implementation("androidx.activity:activity-ktx:1.9.3")
    implementation("com.google.android.material:material:1.12.0")
    testImplementation("junit:junit:4.13.2")
    // The android.jar org.json stubs return defaults in unit tests; the real parser keeps the
    // HELLO payload test honest.
    testImplementation("org.json:json:20231013")
}
