"""Acquire exact public corpus inputs in CI. Never load a model."""
import argparse
import subprocess
import urllib.request
from pathlib import Path

import build_accuracy_corpus as corpus


REVISION = "3c4b60807d71f79b43f3c4363786d9493691f8b1"
FILES = ("tokenizer.json", "tokenizer_config.json", "special_tokens_map.json",
         "vocab.txt", "config.json")


def acquire(manifest: Path, pin: str, output: Path) -> None:
    sources = corpus.input_manifest(manifest, pin)["sources"]
    config = corpus.read_json(Path(__file__).resolve().parents[1]
                              / "pod-cost/cpu-cost-v1.json")
    if config["model_revision"] != REVISION:
        raise corpus.CorpusError("CodeRankEmbed revision changed")
    pins = {name: config["inputs"]["data/cpu-cost/tokenizer/" + name]
            for name in FILES}
    output.mkdir()  # No prior files or model caches may supply these bytes.
    tokenizer = output / "tokenizer"
    tokenizer.mkdir()
    for name in FILES:
        url = ("https://huggingface.co/nomic-ai/CodeRankEmbed/resolve/"
               f"{REVISION}/{name}")
        # No authentication headers, huggingface_hub, model weights or caches.
        with urllib.request.urlopen(url, timeout=60) as response:
            content = response.read(8 * 1024 * 1024 + 1)
        if (len(content) > 8 * 1024 * 1024
                or corpus.digest(content) != pins[name]):
            raise corpus.CorpusError("tokenizer hash/size mismatch: " + name)
        with (tokenizer / name).open("xb") as stream:
            stream.write(content)
    for source in sources:
        directory = output / source["language"]
        directory.mkdir()
        commands = [
            ["git", "init", "--quiet", str(directory)],
            ["git", "-C", str(directory), "fetch", "--depth", "1", "--no-tags",
             source["repo"] + ".git", source["commit"]],
            ["git", "-C", str(directory), "-c", "core.hooksPath=/dev/null",
             "checkout", "--detach", source["commit"]],
        ]
        for command in commands:
            subprocess.run(command, check=True, timeout=180)
        for expression, expected in (("HEAD", source["commit"]),
                                     ("HEAD^{tree}", source["tree"])):
            actual = corpus.git_output(directory, "rev-parse", expression)
            if actual.decode().strip() != expected:
                raise corpus.CorpusError("fetched commit/tree mismatch")
    corpus.write_json(output / "acquisition.json", {
        "schema": "accuracy-acquisition/v1", "sources": sources,
        "model_revision": REVISION, "tokenizer_sha256": pins,
        "manifest_sha256": pin})


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--manifest", type=Path, required=True)
    parser.add_argument("--manifest-pin", required=True)
    parser.add_argument("--output", type=Path, required=True)
    args = parser.parse_args()
    acquire(args.manifest, args.manifest_pin, args.output)
