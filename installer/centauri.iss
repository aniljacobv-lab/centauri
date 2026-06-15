; Inno Setup script for the Centauri Windows installer.
; Build: install Inno Setup (jrsoftware.org/isinfo.php), then either open
; this file in the Inno IDE and click Compile, or run:
;   iscc installer\centauri.iss
; Prereq: release.bat has produced dist\centauri-<ver>-windows-amd64.exe
; (adjust Source below if your version tag differs).

#define MyAppName "Centauri"
; release.bat passes the version via ISCC /DMyAppVersion=...; this is the fallback.
#ifndef MyAppVersion
  #define MyAppVersion "0.3.0"
#endif
#define MyAppPublisher "Proxima360"
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

[Files]
Source: "..\dist\centauri-windows-amd64.exe"; DestDir: "{app}"; DestName: "centauri.exe"; Flags: ignoreversion

[Icons]
; The shortcut runs `centauri desktop`: data goes to %APPDATA%\Centauri,
; the browser opens automatically — no localhost confusion.
Name: "{group}\Centauri"; Filename: "{app}\centauri.exe"; Parameters: "desktop"; Comment: "The database that never forgets"
Name: "{autodesktop}\Centauri"; Filename: "{app}\centauri.exe"; Parameters: "desktop"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "Create a &desktop icon"; GroupDescription: "Additional icons:"
Name: "addtopath"; Description: "Add Centauri to your PATH (use 'centauri' in any terminal)"; GroupDescription: "Command line:"

[Registry]
Root: HKA; Subkey: "Environment"; ValueType: expandsz; ValueName: "Path"; ValueData: "{olddata};{app}"; Tasks: addtopath; Check: NeedsAddPath('{app}')

[Run]
Filename: "{app}\centauri.exe"; Parameters: "desktop"; Description: "Start Centauri now"; Flags: nowait postinstall skipifsilent

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
