package main

import rego.v1

# input.writ is what writ serve adds to the plan document. conftest reads
# the output of `tofu show -json`, which has no writ key, so there these
# rules see no environment and no decision point.
environment := object.get(input, ["writ", "environment"], "")

decision := object.get(input, ["writ", "decision"], "")
