#define AppName "PhoneBridge"
#define AppVersion "2.1.1"
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
SetupIconFile=..\assets\branding\phonebridge.ico

[Languages]
Name: "chinesesimp"; MessagesFile: "compiler:Languages\\ChineseSimplified.isl"

[Files]
Source: "..\dist\PhoneBridge\PhoneBridge.exe"; DestDir: "{app}"; Flags: ignoreversion
Source: "..\dist\PhoneBridge\runtime\scrcpy\*"; DestDir: "{app}\runtime\scrcpy"; Flags: ignoreversion recursesubdirs createallsubdirs
Source: "..\README.md"; DestDir: "{app}"; DestName: "README.md"; Flags: ignoreversion
Source: "..\assets\branding\phonebridge.ico"; DestDir: "{app}"; DestName: "phonebridge.ico"; Flags: ignoreversion

[Icons]
Name: "{autoprograms}\PhoneBridge\打开 PhoneBridge"; Filename: "{app}\PhoneBridge.exe"; Parameters: "--open-browser"; WorkingDir: "{app}"; IconFilename: "{app}\phonebridge.ico"
Name: "{autodesktop}\PhoneBridge"; Filename: "{app}\PhoneBridge.exe"; Parameters: "--open-browser"; WorkingDir: "{app}"; IconFilename: "{app}\phonebridge.ico"; Tasks: desktopicon
Name: "{autostartup}\PhoneBridge"; Filename: "{app}\PhoneBridge.exe"; WorkingDir: "{app}"; IconFilename: "{app}\phonebridge.ico"; Comment: "登录 Windows 时在后台启动 PhoneBridge"; Tasks: autostart

[Tasks]
Name: "autostart"; Description: "登录 Windows 时在后台启动 PhoneBridge"; GroupDescription: "启动选项："; Flags: checkedonce
Name: "desktopicon"; Description: "创建桌面快捷方式"; GroupDescription: "附加图标："; Flags: unchecked

[UninstallRun]
Filename: "{sys}\taskkill.exe"; Parameters: "/F /IM PhoneBridge.exe"; Flags: runhidden; RunOnceId: "StopPhoneBridge"

[Code]
function PrepareToInstall(var NeedsRestart: Boolean): String;
var
  ResultCode: Integer;
  InstalledADB: String;
begin
  { Stop the old host before files are replaced. Its windowless background
    process is not always discovered by Restart Manager. }
  Exec(ExpandConstant('{sys}\taskkill.exe'), '/F /IM PhoneBridge.exe',
    '', SW_HIDE, ewWaitUntilTerminated, ResultCode);

  { Ask the installed ADB client to release port 5037. This avoids killing
    unrelated applications and prevents adb.exe from locking its own folder. }
  InstalledADB := ExpandConstant('{app}\runtime\scrcpy\adb.exe');
  if FileExists(InstalledADB) then
    Exec(InstalledADB, 'kill-server', ExtractFileDir(InstalledADB),
      SW_HIDE, ewWaitUntilTerminated, ResultCode);
  Sleep(400);
  Result := '';
end;

{ Launch the freshly installed host after every successful install, whether it
  was a visible install, a silent (/SILENT or /VERYSILENT) install, or a silent
  upgrade. This replaced the old [Run] entry whose silent-skip flag silently
  disabled the launch during unattended installs. A single launch here (never
  paired with a [Run] entry) guarantees at most one PhoneBridge instance, and
  it runs only after files were copied, the old process was stopped and the
  ADB kill-server finished — PrepareToInstall and ssPostInstall ordering. }
procedure CurStepChanged(CurStep: TSetupStep);
var
  ResultCode: Integer;
  Params: String;
begin
  if CurStep = ssPostInstall then
  begin
    if WizardSilent then
      Params := ''
    else
      Params := '--open-browser';
    Exec(ExpandConstant('{app}\PhoneBridge.exe'), Params,
      ExpandConstant('{app}'), SW_HIDE, ewNoWait, ResultCode);
  end;
end;
