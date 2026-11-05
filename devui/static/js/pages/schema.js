import intelligence from './intelligence.js';

export default function(root) {
  return intelligence(root, [], { settingsOnly: true });
}
