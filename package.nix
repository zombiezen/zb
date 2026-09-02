{ buildGoModule
, lib
, installShellFiles
}:

let
  version = "0.2.0-beta6";
in

buildGoModule {
  pname = "zb";
  version = version;

  ldflags = [
    "-s -w -X main.zbVersion=${version}"
  ];
  tags = [
    "disable_grpc_modules"
  ];

  nativeBuildInputs = [
    installShellFiles
  ];

  src = ./.;

  vendorHash = "sha256-Yxh0qbyiUIBRX06yZQDQJqNhnA+dp2zGQO2ihuawkzU=";
  goSum = builtins.readFile ./go.sum;

  postInstall = ''
    installShellCompletion --cmd zb \
      --bash <($out/bin/zb completion -c bash) \
      --fish <($out/bin/zb completion -c fish) \
      --zsh <($out/bin/zb completion -c zsh)
  '';

  meta = {
    description = "An experiment in hermetic, reproducible build systems.";
    homepage = "https://zb.256lights.llc/";
    downloadPage = "https://github.com/256lights/zb/releases/latest";
    license = lib.licenses.mit;
  };
}
