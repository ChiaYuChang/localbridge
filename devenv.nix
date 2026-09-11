{
  pkgs,
  lib,
  config,
  inputs,
  ...
}:

{
  env = {
    GREET = "devenv";
  };

  packages = [
    pkgs.git
    pkgs.go-task
  ];

  languages.go.enable = true;

  scripts.hello.exec = ''
    echo hello from $GREET
  '';

  enterShell = ''
    go version
    git --version
  '';
}
