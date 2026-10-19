// SPDX-License-Identifier: MIT
// @ts-check

/** @type {import('@docusaurus/plugin-content-docs').SidebarsConfig} */
const sidebars = {
  guidesSidebar: [
    "intro",
    "onboarding",
    {
      type: "category",
      label: "Platform surfaces",
      items: ["webhooks", "mini-apps"],
    },
  ],
};

module.exports = sidebars;
