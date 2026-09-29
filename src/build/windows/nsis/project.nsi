Unicode true

####
## Please note: Template replacements don't work in this file. They are provided with default defines like
## mentioned underneath.
## If the keyword is not defined, "wails_tools.nsh" will populate them.
## If they are defined here, "wails_tools.nsh" will not touch them. This allows you to use this project.nsi manually
## from outside of Wails for debugging and development of the installer.
## 
## For development first make a wails nsis build to populate the "wails_tools.nsh":
## > wails build --target windows/amd64 --nsis
## Then you can call makensis on this file with specifying the path to your binary:
## For a AMD64 only installer:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\..\..\bin\app.exe
## For a ARM64 only installer:
## > makensis -DARG_WAILS_ARM64_BINARY=..\..\..\..\bin\app.exe
## For a installer with both architectures:
## > makensis -DARG_WAILS_AMD64_BINARY=..\..\..\..\bin\app-amd64.exe -DARG_WAILS_ARM64_BINARY=..\..\..\..\bin\app-arm64.exe
####
## The following information is taken from the wails_tools.nsh file, but they can be overwritten here.
####
## !define INFO_PROJECTNAME    "Quarry" # Default "Quarry"
## !define INFO_COMPANYNAME    "RCooLeR" # Default "RCooLeR"
## !define INFO_PRODUCTNAME    "Quarry" # Default "Quarry"
## !define INFO_PRODUCTVERSION "0.0.0-dev" # Linker/release metadata overrides this
## !define INFO_NUMERICVERSION "0.0.0"     # Numeric MAJOR.MINOR.PATCH
## !define INFO_BUILDNUMBER    "123"       # Monotonic numeric release build
## !define INFO_COPYRIGHT      "(c) 2026, RCooLeR" # Default "© 2026, RCooLeR"
###
## !define PRODUCT_EXECUTABLE  "quarry.exe"           # Default "${INFO_PROJECTNAME}.exe"
## !define UNINST_KEY_NAME     "UninstKeyInRegistry"  # Default "${INFO_COMPANYNAME}${INFO_PRODUCTNAME}"
####
## !define REQUEST_EXECUTION_LEVEL "admin"            # Default "admin"  see also https://nsis.sourceforge.io/Docs/Chapter4.html
## !define WAILS_INSTALL_SCOPE     "user"             # Default "machine" - set to "user" for per-user install ($LOCALAPPDATA) without UAC prompt
####
## Include the wails tools
####
!include "wails_tools.nsh"

!ifndef INFO_NUMERICVERSION
    !define INFO_NUMERICVERSION "${INFO_PRODUCTVERSION}"
!endif
!ifndef INFO_BUILDNUMBER
    !define INFO_BUILDNUMBER "0"
!endif

# The version information for this two must consist of 4 parts
VIProductVersion "${INFO_NUMERICVERSION}.${INFO_BUILDNUMBER}"
VIFileVersion    "${INFO_NUMERICVERSION}.${INFO_BUILDNUMBER}"

VIAddVersionKey "CompanyName"     "${INFO_COMPANYNAME}"
VIAddVersionKey "FileDescription" "${INFO_PRODUCTNAME} Installer"
VIAddVersionKey "ProductVersion"  "${INFO_PRODUCTVERSION}"
VIAddVersionKey "FileVersion"     "${INFO_PRODUCTVERSION}"
VIAddVersionKey "LegalCopyright"  "${INFO_COPYRIGHT}"
VIAddVersionKey "ProductName"     "${INFO_PRODUCTNAME}"

# Enable HiDPI support. https://nsis.sourceforge.io/Reference/ManifestDPIAware
ManifestDPIAware true

!include "MUI.nsh"

!define MUI_ICON "..\icon.ico"
!define MUI_UNICON "..\icon.ico"
# !define MUI_WELCOMEFINISHPAGE_BITMAP "resources\leftimage.bmp" #Include this to add a bitmap on the left side of the Welcome Page. Must be a size of 164x314
!define MUI_FINISHPAGE_NOAUTOCLOSE # Wait on the INSTFILES page so the user can take a look into the details of the installation steps
!define MUI_ABORTWARNING # This will warn the user if they exit from the installer.

!insertmacro MUI_PAGE_WELCOME # Welcome to the installer page.
# !insertmacro MUI_PAGE_LICENSE "resources\eula.txt" # Adds a EULA page to the installer
!insertmacro MUI_PAGE_DIRECTORY # In which folder install page.
!insertmacro MUI_PAGE_INSTFILES # Installing page.
!insertmacro MUI_PAGE_FINISH # Finished installation page.

!insertmacro MUI_UNPAGE_INSTFILES # Uninstalling page

!insertmacro MUI_LANGUAGE "English" # Set the Language of the installer

# Quarry requires the Evergreen WebView2 Runtime. The online bootstrapper is
# verified as Microsoft-signed by the build task before it is embedded. If the
# runtime is absent, bootstrap failure aborts before Quarry files, shortcuts, or
# uninstall metadata are written.
!macro quarry.requireWebView2
    SetRegView 64
    ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
    # Edge Update uses 0.0.0.0 as an explicit "not installed" sentinel.
    # Normalize it before the user-scope fallback so an HKCU runtime can still
    # satisfy a per-user install when the machine key contains that sentinel.
    ${If} $0 == "0.0.0.0"
        StrCpy $0 ""
    ${EndIf}

    !if "${WAILS_INSTALL_SCOPE}" == "user"
        ${If} $0 == ""
            ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
            ${If} $0 == "0.0.0.0"
                StrCpy $0 ""
            ${EndIf}
        ${EndIf}
    !endif

    ${If} $0 == ""
        SetDetailsPrint both
        DetailPrint "Microsoft WebView2 Runtime is required; running the signed online bootstrapper"
        SetDetailsPrint listonly

        InitPluginsDir
        CreateDirectory "$pluginsdir\webview2bootstrapper"
        SetOutPath "$pluginsdir\webview2bootstrapper"
        File "MicrosoftEdgeWebview2Setup.exe"
        ExecWait '"$pluginsdir\webview2bootstrapper\MicrosoftEdgeWebview2Setup.exe" /silent /install' $1

        ${If} $1 != 0
            StrCpy $2 "Microsoft WebView2 Runtime setup failed with exit code $1. Quarry was not installed. Check network access and installer permissions, then retry."
            Goto quarry_webview_failed
        ${EndIf}

        ReadRegStr $0 HKLM "SOFTWARE\WOW6432Node\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
        ${If} $0 == "0.0.0.0"
            StrCpy $0 ""
        ${EndIf}
        !if "${WAILS_INSTALL_SCOPE}" == "user"
            ${If} $0 == ""
                ReadRegStr $0 HKCU "Software\Microsoft\EdgeUpdate\Clients\{F3017226-FE2A-4295-8BDF-00C3A9A7E4C5}" "pv"
                ${If} $0 == "0.0.0.0"
                    StrCpy $0 ""
                ${EndIf}
            ${EndIf}
        !endif
        ${If} $0 == ""
            StrCpy $2 "Microsoft WebView2 Runtime setup did not produce a detectable runtime. Quarry was not installed."
            Goto quarry_webview_failed
        ${EndIf}
    ${EndIf}
    Goto quarry_webview_ready

    quarry_webview_failed:
        SetErrorLevel 66
        IfSilent quarry_webview_quit quarry_webview_message
    quarry_webview_message:
        MessageBox MB_OK|MB_ICONSTOP "$2"
    quarry_webview_quit:
        Quit
    quarry_webview_ready:
        SetDetailsPrint both
!macroend

## The following two statements can be used to sign the installer and the uninstaller. The path to the binaries are provided in %1
#!uninstfinalize 'signtool --file "%1"'
#!finalize 'signtool --file "%1"'

Name "${INFO_PRODUCTNAME}"
OutFile "..\..\..\..\bin\${INFO_PROJECTNAME}-${ARCH}-installer.exe" # Repository-level binary output.
!if "${WAILS_INSTALL_SCOPE}" == "user"
    InstallDir "$LOCALAPPDATA\Programs\${INFO_PRODUCTNAME}"
!else
    InstallDir "$PROGRAMFILES64\${INFO_COMPANYNAME}\${INFO_PRODUCTNAME}"
!endif
ShowInstDetails show # This will always show the installation details.

Function .onInit
   !insertmacro wails.checkArchitecture
FunctionEnd

Section
    !insertmacro wails.setShellContext

    !insertmacro quarry.requireWebView2

    SetOutPath $INSTDIR
    
    !insertmacro wails.files

    CreateShortcut "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"
    CreateShortCut "$DESKTOP\${INFO_PRODUCTNAME}.lnk" "$INSTDIR\${PRODUCT_EXECUTABLE}"

    !insertmacro wails.associateFiles
    !insertmacro wails.associateCustomProtocols
    
    !insertmacro wails.writeUninstaller
SectionEnd

Section "uninstall" 
    !insertmacro wails.setShellContext

    Delete "$SMPROGRAMS\${INFO_PRODUCTNAME}.lnk"
    Delete "$DESKTOP\${INFO_PRODUCTNAME}.lnk"

    !insertmacro wails.unassociateFiles
    !insertmacro wails.unassociateCustomProtocols

    # Remove only files the installer owns. Quarry settings, the WebView2
    # profile, source files, recovery evidence, and unknown files are preserved.
    Delete "$INSTDIR\${PRODUCT_EXECUTABLE}"
    !insertmacro wails.deleteUninstaller
    RMDir "$INSTDIR"
SectionEnd
