@echo off
rem build.bat - construit P4Relay.exe en application Windows : sous-systeme
rem GUI, donc AUCUNE fenetre de commande au double-clic ; seule l'interface
rem apparait. Lance depuis un terminal, l'exe y rattache la console et affiche
rem ses messages.
rem
rem Les cibles non Windows se compilent sans ce fichier :
rem   go build -o P4Relay ./cmd/p4relay
setlocal
cd /d "%~dp0"
set GOEXE=go
where go >nul 2>nul || set GOEXE=C:\ProgramData\ajean\workspace\go1.27.1\bin\go.exe
"%GOEXE%" build -ldflags "-H windowsgui -s -w" -o P4Relay.exe ./cmd/p4relay
if errorlevel 1 goto echec
echo P4Relay.exe construit en application Windows, sans fenetre de commande.
exit /b 0
:echec
echo Echec de la compilation.
exit /b 1
