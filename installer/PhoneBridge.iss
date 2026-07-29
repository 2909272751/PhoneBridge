#define AppName "PhoneBridge"
#define AppVersion "1.1.2"
#define AppPublisher "PhoneBridge"

[Setup]
AppId={{DEB318B0-AC03-49E0-94C4-80F7E50D8399}
AppName={#AppName}
AppVersion={#AppVersion}
AppPublisher={#AppPublisher}
; First install: use Windows' standard program directory, while keeping the
; directory page available so every computer may choose another drive.
; Upgrade: the stable AppId lets Inno Setup discover the previous AppDir and
; overwrite that installation instead of silently creating a second copy.
DefaultDirName={autopf}\PhoneBridge
UsePreviousAppDir=yes
DefaultGroupName=PhoneBridge
DisableProgramGroupPage=yes
OutputDir=..\dist\installer
OutputBaseFilename=PhoneBridge-Setup-{#AppVersion}-Windows-x64
Compression=lzma2
SolidCompression=yes
WizardStyle=modern
ArchitecturesAllowed=x64compatible
ArchitecturesInstallIn64BitMode=x64compatible
UninstallDisplayName=PhoneBridge

[Languages]
Name: "chinesesimp"; MessagesFile: "compiler:Languages\\ChineseSimplified.isl"

[Files]
Source: "..\dist\PhoneBridge\PhoneBridge.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\dist\PhoneBridge\runtime\scrcpy\*"; DestDir: "{app}\runtime\scrcpy"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "..\README.md"; DestDir: "{app}"; DestName: "README.md"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\PhoneBridge\打开 PhoneBridge"; Filename: "{app}\PhoneBridge.exe"; Parameters: "--open-browser"; WorkingDir: "{app}"
Name: "{autodesktop}\PhoneBridge"; Filename: "{app}\PhoneBridge.exe"; Parameters: "--open-browser"; WorkingDir: "{app}"; Tasks: desktopicon

[Tasks]
Name: "desktopicon"; Description: "创建桌面快捷方式"; GroupDescription: "附加图标："; Flags: unchecked

[Run]
Filename: "{app}\PhoneBridge.exe"; Parameters: "--open-browser"; WorkingDir: "{app}"; Description: "立即启动 PhoneBridge"; Flags: nowait postinstall skipifsilent runhidden

[UninstallRun]
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM PhoneBridge.exe"; Flags: runhidden; RunOnceId: "StopPhoneBridge"
