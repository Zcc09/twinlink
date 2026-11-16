@echo off
REM Twinlink local Android build entry point (persistent toolchain under C:\Users\Zcc09\tools).
set "JAVA_HOME=C:\Users\Zcc09\tools\re\jdk21"
set "ANDROID_HOME=C:\Users\Zcc09\tools\android-sdk"
set "ANDROID_SDK_ROOT=%ANDROID_HOME%"
set "GRADLE_BIN=C:\Users\Zcc09\tools\gradle-9.7.1\bin\gradle.bat"
cd /d C:\Users\Zcc09\twinlink\app
if "%~1"=="" (
  call "%GRADLE_BIN%" --no-daemon :app:testDebugUnitTest :app:assembleDebug
) else (
  call "%GRADLE_BIN%" --no-daemon %*
)
