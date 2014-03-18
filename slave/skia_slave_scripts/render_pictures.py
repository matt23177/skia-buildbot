#!/usr/bin/env python
# Copyright (c) 2013 The Chromium Authors. All rights reserved.
# Use of this source code is governed by a BSD-style license that can be
# found in the LICENSE file.

""" Run the Skia render_pictures executable. """

from build_step import BuildStep
import os
import sys


DEFAULT_TILE_X = 256
DEFAULT_TILE_Y = 256


class RenderPictures(BuildStep):
  def __init__(self, timeout=18000, no_output_timeout=9600, **kwargs):
    super(RenderPictures, self).__init__(
      timeout=timeout, no_output_timeout=no_output_timeout, **kwargs)

  def DoRenderPictures(self, args, config='8888', write_images=True):
    # For now, don't run on Android, since it takes too long and we don't use
    # the results.
    if 'Android' in self._builder_name:
      return
    cmd = ['-r', self._device_dirs.SKPDir(), '--config', config,
           '--mode', 'tile', str(DEFAULT_TILE_X), str(DEFAULT_TILE_Y)]
    cmd.extend(args)
    if False:
      # For now, skip --validate and writing images on all builders, since they
      # take too long and we aren't making use of them.
      # Also skip --validate on Windows, where it is currently failing.
      if write_images:
        cmd.extend(['-w', self._device_dirs.SKPOutDir()])
      if not os.name == 'nt':
        cmd.append('--validate')
    self._flavor_utils.RunFlavoredCmd('render_pictures', cmd)

  def _Run(self):
    self.DoRenderPictures([])
    self.DoRenderPictures(['--bbh', 'grid', str(DEFAULT_TILE_X),
                           str(DEFAULT_TILE_X), '--clone', '1'])
    self.DoRenderPictures(['--bbh', 'rtree', '--clone', '2'])
    self.DoRenderPictures(['--deferImageDecoding', '--useVolatileCache'])


if '__main__' == __name__:
  sys.exit(BuildStep.RunBuildStep(RenderPictures))
