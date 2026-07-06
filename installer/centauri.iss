; Inno Setup script for the Centauri Windows installer.
; Build: install Inno Setup (jrsoftware.org/isinfo.php), then either open
; this file in the Inno IDE and click Compile, or run:
;   iscc installer\centauri.iss
; Prereq: release.bat has produced dist\centauri-windows-amd64.exe
; (adjust Source below if your version tag differs).
;
; The experience this script promises: install -> Start Menu / desktop
; shortcut -> double-click -> working local AI. The shortcut runs
; `centauri desktop`, which stores data in %APPDATA%\Centauri, opens the
; browser at /app, and sets up the local AI on first run (one-time model
; download). No terminal knowledge required.

#define MyAppName "Centauri"
; release.bat passes the version via ISCC /DMyAppVersion=...; this is the fallback.
#ifndef MyAppVersion
  #define MyAppVersion "0.3.0"
#endif
#define MyAppPublisher "JacobLabs LLC"
#define MyAppURL "https://github.com/aniljacobv-lab/centauri"

[Setup]
AppId={{8C9A2C51-46F1-4E1B-9B0D-CENTAURI0301}
AppName={#MyAppName}
AppVersion={#MyAppVersion}
AppPublisher={#MyAppPublisher}
AppPublisherURL={#MyAppURL}
AppSupportURL={#MyAppURL}/issues
DefaultDirName={autopf}\Centauri
DefaultGroupName=Centauri
DisableProgramGroupPage=yes
OutputDir=..\dist
OutputBaseFilename=centauri-windows-setup
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
PrivilegesRequiredOverridesAllowed=dialog
UninstallDisplayIcon={app}\centauri.exe
; Note: your data lives in %APPDATA%\Centauri (not in {app}), so
; uninstalling the program never touches your facts. Nothing is ever erased.

[Files]
Source: "..\dist\centauri-windows-amd64.exe"; DestDir: "{app}"; DestName: "centauri.exe"; Flags: ignoreversion
Source: "..\README.md"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\docs\quickstart.md"; DestDir: "{app}"; DestName: "QUICKSTART.md"; Flags: ignoreversion

[Icons]
; The shortcut runs `centauri desktop`: data goes to %APPDATA%\Centauri,
; the browser opens automatically - no localhost confusion.
Name: "{group}\Centauri"; Filename: "{app}\centauri.exe"; Parameters: "desktop"; Comment: "Your private AI - runs on this computer, opens in your browser"
Name: "{group}\Quickstart guide"; Filename: "notepad.exe"; Parameters: """{app}\QUICKSTART.md"""; Comment: "Plain-English guide: install, first run, asking questions"
Name: "{autodesktop}\Centauri"; Filename: "{app}\centauri.exe"; Parameters: "desktop"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "Create a &desktop icon"; GroupDescription: "Additional icons:"
Name: "addtopath"; Description: "Add Centauri to your PATH (use 'centauri' in any terminal)"; GroupDescription: "Command line:"; Flags: unchecked

[Registry]
Root: HKA; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Tasks: addtopath; Check: NeedsAddPath('{app}')

[Run]
Filename: "{app}\centauri.exe"; Parameters: "desktop"; Description: "Start Centauri now (your private AI finishes setting up on first run)"; Flags: nowait postinstall skipifsilent

[Messages]
FinishedLabel=Setup has finished installing [name] on your computer.%n%nWhen you start Centauri, a small console window opens (that's the engine - keep it open) and the app appears in your browser. Your private AI sets itself up on the first run - the first model download can take a few minutes. Everything stays on this computer.

[Code]
function NeedsAddPath(Param: string): boolean;
var
  OrigPath: string;
begin
  if not RegQueryStringValue(HKA, 'Environment', 'Path', OrigPath) then
  begin
    Result := True;
    exit;
  end;
  Result := Pos(';' + ExpandConstant(Param) + ';', ';' + OrigPath + ';') = 0;
end;
