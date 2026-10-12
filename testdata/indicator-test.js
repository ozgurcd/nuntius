const fs = require('node:fs'), vm = require('node:vm');
const script = fs.readFileSync('app.js', 'utf8');
const fn = script.slice(script.indexOf('function laneIndicator('), script.indexOf('\nfunction renderAgentTabs('));
const ctx = vm.createContext({}); vm.runInContext(fn, ctx);
const cases = [
 [{state:'idle'},'idle'], [{state:'running'},'running'],
 [{state:'ended'},'finished'], [{state:'completed'},'finished'],
 [{state:'running',display_state:'stuck?'},'stuck'],
 [{state:'running',last_event_age_seconds:901},'stuck'],
 [{state:'running',last_event_age_seconds:900},'running'],
 [{state:'ended',exit:1},'failed'], [{state:'ended_no_report'},'failed'],
 [{state:'interrupted'},'failed'], [{state:'failed'},'failed'],
 [{state:'ended',display_state:'Ended · resumed 1/2'},'finished'],
 [{state:'ended_no_report',display_state:'Ended · resumed 1/2',badge:'amber'},'failed'],
 [{state:'running',display_state:'Running · resumed 1/2'},'running'],
 [{state:'ended',display_state:'Failed · resumed 1/2'},'failed'],
 [{state:'ended_no_report',outcome:'report_delivery_failed',display_state:'Ended · report missing',badge:'amber'},'attention'],
 [{state:'ended_no_report',outcome:'report_delivery_failed',display_state:'Ended · report missing · resumed 1/2',badge:'amber'},'attention']
];
for (const [s,want] of cases) {
 const value = ctx.laneIndicator(s);
 if (value.kind !== want || !value.label) throw new Error('lane indicator mapping');
}
console.log('lane indicator mapping passed');

// Offline layout contracts complement the DOM runtime tests; they do not claim
// to measure Safari geometry. Inspect the final phone rule to catch cascade drift.
const css = fs.readFileSync('app.css', 'utf8');
const phone = [...css.matchAll(/@media\s*\(max-width:\s*720px\)\s*\{([^\n]*)/g)].at(-1)?.[1] || '';
const failures = [];
if (!/\.topbar\{[^}]*position:static/.test(phone)) failures.push('G3 narrow and zoomed header scrolls with content');
if (!/\.topbar\{[^}]*grid-template-columns:minmax\(0,1fr\)/.test(phone) || !/\.dispatch\{[^}]*grid-column:1\s*\/\s*-1[^}]*flex-wrap:wrap/.test(phone)) failures.push('G7 brand and controls occupy separate wrapping rows');
if (!/\.agent-card \.kind\{[^}]*text-transform:none/.test(css)) failures.push('G4 model and effort keep their exact case');
if (!/\.agent-card \.badge:not\(\.green\):not\(\.amber\):not\(\.red\)\{color:var\(--text\)/.test(css)) failures.push('G10 neutral card badge uses high-contrast text');
function luminance(hex) { return hex.match(/[\da-f]{2}/gi).map(c => parseInt(c,16)/255).map(c => c <= .04045 ? c/12.92 : ((c+.055)/1.055)**2.4).reduce((v,c,i) => v+c*[.2126,.7152,.0722][i],0); }
const dark = css.match(/@media\(prefers-color-scheme:dark\)\{:(?:root)\{([^}]+)/)?.[1] || '';
const text = dark.match(/--text:#([\da-f]{6})/)?.[1], soft = dark.match(/--soft:#([\da-f]{6})/)?.[1];
if (!text || !soft || (luminance(text)+.05)/(luminance(soft)+.05) < 4.5) failures.push('G10 dark neutral badge text meets AA 4.5:1');
if (failures.length) throw new Error(failures.join('; '));
console.log('dashboard phone layout and dark badge contrast contracts passed');
