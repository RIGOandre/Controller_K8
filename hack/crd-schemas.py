#!/usr/bin/env python3
"""Extrai o schema de cada CRD para o formato que o kubeconform lê.

Sem isso o kubeconform pula o sample do PreviewEnvironment: ele só conhece os
tipos nativos, e um recurso pulado passa no CI parecendo aprovado. Um campo
digitado errado no exemplo chegaria ao cluster.

O schema sai mais rígido do que o do apiserver de propósito. Diante de um
campo que não existe o apiserver poda em silêncio e aplica o resto; aqui o CI
reprova e diz o nome do campo.
"""

import json
import pathlib
import sys

import yaml

ORIGEM = pathlib.Path("config/crd/bases")
DESTINO = pathlib.Path(".schemas")


def fechar(no: object) -> None:
    """Fecha todo objeto do schema para campo desconhecido, recursivamente.

    Só mexe em nó que declara `properties` e ainda não diz nada sobre
    additionalProperties. Mapa livre (`additionalProperties: {type: string}`,
    que é como labels e nodeSelector chegam aqui) já vem decidido e fica como
    está, e nó com x-kubernetes-preserve-unknown-fields é intocado porque a
    liberdade ali é intencional.
    """
    if isinstance(no, list):
        for item in no:
            fechar(item)
        return
    if not isinstance(no, dict):
        return

    if (
        "properties" in no
        and "additionalProperties" not in no
        and not no.get("x-kubernetes-preserve-unknown-fields")
    ):
        no["additionalProperties"] = False

    for chave, valor in no.items():
        if chave != "additionalProperties" or isinstance(valor, dict):
            fechar(valor)


def main() -> int:
    arquivos = sorted(ORIGEM.glob("*.yaml"))
    if not arquivos:
        print(f"nenhum CRD em {ORIGEM}", file=sys.stderr)
        return 1

    DESTINO.mkdir(exist_ok=True)
    escritos = 0
    for arquivo in arquivos:
        for doc in yaml.safe_load_all(arquivo.read_text()):
            if not doc or doc.get("kind") != "CustomResourceDefinition":
                continue
            kind = doc["spec"]["names"]["kind"]
            for versao in doc["spec"]["versions"]:
                schema = versao.get("schema", {}).get("openAPIV3Schema")
                if schema is None:
                    continue
                fechar(schema)
                alvo = DESTINO / f"{kind.lower()}_{versao['name']}.json"
                alvo.write_text(json.dumps(schema, indent=2) + "\n")
                print(alvo)
                escritos += 1

    if escritos == 0:
        print("nenhum schema extraído", file=sys.stderr)
        return 1
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
