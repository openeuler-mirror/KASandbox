#!/bin/bash

rm -rf e2b/gsd/__pycache__
rm -rf e2b/gsd/checkpoint/__pycache__

sed -i.bak 's/from\ checkpoint\ import/from e2b.gsd.checkpoint import/g' e2b/gsd/checkpoint/*

rm -f e2b/gsd/checkpoint/*.bak