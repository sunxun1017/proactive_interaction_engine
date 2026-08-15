# Adapted from ModelScope 3D-Speaker's ERes2Net_huge, fusion, and pooling code.
# Copyright 3D-Speaker contributors. Licensed under Apache-2.0; see
# workers/speaker/THIRD_PARTY_NOTICES.md.

import math

import torch
import torch.nn as nn
import torch.nn.functional as functional


class ReLU(nn.Hardtanh):
    def __init__(self, inplace=False):
        super().__init__(0, 20, inplace)


class AFF(nn.Module):
    def __init__(self, channels=64, reduction=4):
        super().__init__()
        intermediate_channels = channels // reduction
        self.local_att = nn.Sequential(
            nn.Conv2d(channels * 2, intermediate_channels, kernel_size=1),
            nn.BatchNorm2d(intermediate_channels),
            nn.SiLU(inplace=True),
            nn.Conv2d(intermediate_channels, channels, kernel_size=1),
            nn.BatchNorm2d(channels),
        )

    def forward(self, left, downsampled_right):
        attention = self.local_att(torch.cat((left, downsampled_right), dim=1))
        attention = 1.0 + torch.tanh(attention)
        return left * attention + downsampled_right * (2.0 - attention)


class TSTP(nn.Module):
    def __init__(self, **_kwargs):
        super().__init__()

    def forward(self, value):
        mean = value.mean(dim=-1).flatten(start_dim=1)
        standard_deviation = torch.sqrt(torch.var(value, dim=-1) + 1e-8).flatten(start_dim=1)
        return torch.cat((mean, standard_deviation), dim=1)


class BasicBlockERes2Net(nn.Module):
    expansion = 4

    def __init__(self, in_planes, planes, stride=1, base_width=24, scale=3):
        super().__init__()
        width = int(math.floor(planes * (base_width / 64.0)))
        self.conv1 = nn.Conv2d(in_planes, width * scale, kernel_size=1, stride=stride, bias=False)
        self.bn1 = nn.BatchNorm2d(width * scale)
        self.nums = scale
        self.convs = nn.ModuleList(
            nn.Conv2d(width, width, kernel_size=3, padding=1, bias=False) for _ in range(scale)
        )
        self.bns = nn.ModuleList(nn.BatchNorm2d(width) for _ in range(scale))
        self.relu = ReLU(inplace=True)
        self.conv3 = nn.Conv2d(width * scale, planes * self.expansion, kernel_size=1, bias=False)
        self.bn3 = nn.BatchNorm2d(planes * self.expansion)
        self.shortcut = nn.Sequential()
        if stride != 1 or in_planes != self.expansion * planes:
            self.shortcut = nn.Sequential(
                nn.Conv2d(in_planes, self.expansion * planes, kernel_size=1, stride=stride, bias=False),
                nn.BatchNorm2d(self.expansion * planes),
            )
        self.width = width

    def forward(self, value):
        residual = self.shortcut(value)
        split = torch.split(self.relu(self.bn1(self.conv1(value))), self.width, 1)
        output = None
        current = None
        for index in range(self.nums):
            current = split[index] if index == 0 else current + split[index]
            current = self.relu(self.bns[index](self.convs[index](current)))
            output = current if output is None else torch.cat((output, current), 1)
        return self.relu(self.bn3(self.conv3(output)) + residual)


class FusedBasicBlockERes2Net(nn.Module):
    expansion = 4

    def __init__(self, in_planes, planes, stride=1, base_width=24, scale=3):
        super().__init__()
        width = int(math.floor(planes * (base_width / 64.0)))
        self.conv1 = nn.Conv2d(in_planes, width * scale, kernel_size=1, stride=stride, bias=False)
        self.bn1 = nn.BatchNorm2d(width * scale)
        self.nums = scale
        self.convs = nn.ModuleList(
            nn.Conv2d(width, width, kernel_size=3, padding=1, bias=False) for _ in range(scale)
        )
        self.bns = nn.ModuleList(nn.BatchNorm2d(width) for _ in range(scale))
        self.fuse_models = nn.ModuleList(AFF(channels=width) for _ in range(scale - 1))
        self.relu = ReLU(inplace=True)
        self.conv3 = nn.Conv2d(width * scale, planes * self.expansion, kernel_size=1, bias=False)
        self.bn3 = nn.BatchNorm2d(planes * self.expansion)
        self.shortcut = nn.Sequential()
        if stride != 1 or in_planes != self.expansion * planes:
            self.shortcut = nn.Sequential(
                nn.Conv2d(in_planes, self.expansion * planes, kernel_size=1, stride=stride, bias=False),
                nn.BatchNorm2d(self.expansion * planes),
            )
        self.width = width

    def forward(self, value):
        residual = self.shortcut(value)
        split = torch.split(self.relu(self.bn1(self.conv1(value))), self.width, 1)
        output = None
        current = None
        for index in range(self.nums):
            current = split[index] if index == 0 else self.fuse_models[index - 1](current, split[index])
            current = self.relu(self.bns[index](self.convs[index](current)))
            output = current if output is None else torch.cat((output, current), 1)
        return self.relu(self.bn3(self.conv3(output)) + residual)


class ERes2Net(nn.Module):
    def __init__(
        self,
        block=BasicBlockERes2Net,
        block_fuse=FusedBasicBlockERes2Net,
        num_blocks=(3, 4, 6, 3),
        m_channels=64,
        feat_dim=80,
        embedding_size=192,
    ):
        super().__init__()
        self.in_planes = m_channels
        stats_dim = int(feat_dim / 8) * m_channels * 8
        self.conv1 = nn.Conv2d(1, m_channels, kernel_size=3, stride=1, padding=1, bias=False)
        self.bn1 = nn.BatchNorm2d(m_channels)
        self.layer1 = self._make_layer(block, m_channels, num_blocks[0], stride=1)
        self.layer2 = self._make_layer(block, m_channels * 2, num_blocks[1], stride=2)
        self.layer3 = self._make_layer(block_fuse, m_channels * 4, num_blocks[2], stride=2)
        self.layer4 = self._make_layer(block_fuse, m_channels * 8, num_blocks[3], stride=2)
        self.layer1_downsample = nn.Conv2d(
            m_channels * 4, m_channels * 8, kernel_size=3, padding=1, stride=2, bias=False
        )
        self.layer2_downsample = nn.Conv2d(
            m_channels * 8, m_channels * 16, kernel_size=3, padding=1, stride=2, bias=False
        )
        self.layer3_downsample = nn.Conv2d(
            m_channels * 16, m_channels * 32, kernel_size=3, padding=1, stride=2, bias=False
        )
        self.fuse_mode12 = AFF(channels=m_channels * 8)
        self.fuse_mode123 = AFF(channels=m_channels * 16)
        self.fuse_mode1234 = AFF(channels=m_channels * 32)
        self.pool = TSTP(in_dim=stats_dim * block.expansion)
        self.seg_1 = nn.Linear(stats_dim * block.expansion * 2, embedding_size)
        self.seg_bn_1 = nn.Identity()
        self.seg_2 = nn.Identity()

    def _make_layer(self, block, planes, count, stride):
        layers = []
        for current_stride in (stride,) + (1,) * (count - 1):
            layers.append(block(self.in_planes, planes, current_stride))
            self.in_planes = planes * block.expansion
        return nn.Sequential(*layers)

    def forward(self, value):
        value = value.permute(0, 2, 1).unsqueeze_(1)
        output = functional.relu(self.bn1(self.conv1(value)))
        output1 = self.layer1(output)
        output2 = self.layer2(output1)
        fused12 = self.fuse_mode12(output2, self.layer1_downsample(output1))
        output3 = self.layer3(output2)
        fused123 = self.fuse_mode123(output3, self.layer2_downsample(fused12))
        output4 = self.layer4(output3)
        fused1234 = self.fuse_mode1234(output4, self.layer3_downsample(fused123))
        return self.seg_1(self.pool(fused1234))
