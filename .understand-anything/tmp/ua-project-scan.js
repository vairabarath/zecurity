#!/usr/bin/env node
'use strict';

const fs = require('fs');
const path = require('path');
const { execSync, spawnSync } = require('child_process');

const projectRoot = process.argv[2];
const outputFile = process.argv[3];

if (!projectRoot || !outputFile) {
  process.stderr.write('Usage: node ua-project-scan.js <projectRoot> <outputFile>\n');
  process.exit(1);
}

if (!fs.existsSync(projectRoot)) {
  process.stderr.write(`Project root does not exist: ${projectRoot}\n`);
  process.exit(1);
}

// Step 1: File Discovery via git ls-files
let allFiles = [];
try {
  const result = execSync('git ls-files', { cwd: projectRoot, encoding: 'utf8', maxBuffer: 50 * 1024 * 1024 });
  allFiles = result.split('\n').filter(Boolean);
} catch (e) {
  process.stderr.write(`git ls-files failed: ${e.message}\n`);
  process.exit(1);
}

// Step 2: Exclusion filtering
const EXCLUDED_DIR_SEGMENTS = ['node_modules', '.git', 'vendor', 'venv', '.venv', '__pycache__', 'dist', 'build', 'out', 'coverage', '.next', '.cache', '.turbo', 'target', 'obj', '.idea', '.vscode'];
const EXCLUDED_EXACT = new Set(['LICENSE', '.gitignore', '.editorconfig', '.prettierrc', 'package-lock.json', 'yarn.lock', 'pnpm-lock.yaml']);
const EXCLUDED_EXT = new Set(['.png', '.jpg', '.jpeg', '.gif', '.svg', '.ico', '.woff', '.woff2', '.ttf', '.eot', '.mp3', '.mp4', '.pdf', '.zip', '.tar', '.gz', '.lock']);
const EXCLUDED_PATTERN_EXT = ['.min.js', '.min.css', '.map'];
const EXCLUDED_CONTAINS = ['node_modules/', '.git/', 'vendor/', 'venv/', '.venv/', '__pycache__/', '.idea/', '.vscode/'];

function isExcluded(filePath) {
  const basename = path.basename(filePath);
  if (EXCLUDED_EXACT.has(basename)) return true;
  const ext = path.extname(filePath).toLowerCase();
  if (EXCLUDED_EXT.has(ext)) return true;
  for (const p of EXCLUDED_PATTERN_EXT) {
    if (filePath.endsWith(p)) return true;
  }
  if (/\.generated\./.test(basename)) return true;
  // Check for log files
  if (ext === '.log') return true;
  // eslintrc
  if (/^\.eslintrc/.test(basename)) return true;
  // Check directory segments
  const parts = filePath.split('/');
  for (let i = 0; i < parts.length - 1; i++) {
    if (EXCLUDED_DIR_SEGMENTS.includes(parts[i])) return true;
  }
  return false;
}

let filteredFiles = allFiles.filter(f => !isExcluded(f));

// Step 2.5: .understandignore
let filteredByIgnore = 0;
const understandIgnorePaths = [
  path.join(projectRoot, '.understand-anything', '.understandignore'),
  path.join(projectRoot, '.understandignore'),
];

const ignorePatterns = [];
for (const igPath of understandIgnorePaths) {
  if (fs.existsSync(igPath)) {
    const lines = fs.readFileSync(igPath, 'utf8').split('\n').filter(l => l.trim() && !l.startsWith('#'));
    ignorePatterns.push(...lines);
  }
}

if (ignorePatterns.length > 0) {
  // Simple gitignore-style matching
  function matchesIgnorePattern(filePath, pattern) {
    if (pattern.startsWith('!')) return false; // negation - handled separately
    // Remove leading slash
    const cleanPattern = pattern.startsWith('/') ? pattern.slice(1) : pattern;
    // If pattern ends with /, match as directory prefix
    if (cleanPattern.endsWith('/')) {
      return filePath.startsWith(cleanPattern) || filePath.includes('/' + cleanPattern);
    }
    // Glob-style: if contains *, do simple match
    if (cleanPattern.includes('*')) {
      const regexStr = cleanPattern.replace(/\./g, '\\.').replace(/\*\*/g, '§§').replace(/\*/g, '[^/]*').replace(/§§/g, '.*');
      const regex = new RegExp('(^|/)' + regexStr + '(/|$)');
      return regex.test(filePath);
    }
    // Exact segment or prefix match
    return filePath === cleanPattern || filePath.startsWith(cleanPattern + '/') || filePath.includes('/' + cleanPattern + '/') || filePath.endsWith('/' + cleanPattern);
  }

  const negations = ignorePatterns.filter(p => p.startsWith('!')).map(p => p.slice(1));
  const positives = ignorePatterns.filter(p => !p.startsWith('!'));

  const beforeCount = filteredFiles.length;
  filteredFiles = filteredFiles.filter(f => {
    const shouldIgnore = positives.some(p => matchesIgnorePattern(f, p));
    if (!shouldIgnore) return true;
    // Check if negated
    const negated = negations.some(p => matchesIgnorePattern(f, p));
    return negated;
  });
  filteredByIgnore = beforeCount - filteredFiles.length;
}

// Step 3: Language detection
const EXT_TO_LANG = {
  '.ts': 'typescript', '.tsx': 'typescript',
  '.js': 'javascript', '.jsx': 'javascript',
  '.py': 'python',
  '.go': 'go',
  '.rs': 'rust',
  '.java': 'java',
  '.rb': 'ruby',
  '.cpp': 'cpp', '.cc': 'cpp', '.cxx': 'cpp', '.h': 'cpp', '.hpp': 'cpp',
  '.c': 'c',
  '.cs': 'csharp',
  '.swift': 'swift',
  '.kt': 'kotlin',
  '.php': 'php',
  '.vue': 'vue',
  '.svelte': 'svelte',
  '.sh': 'shell', '.bash': 'shell',
  '.ps1': 'powershell',
  '.bat': 'batch', '.cmd': 'batch',
  '.md': 'markdown', '.rst': 'markdown',
  '.yaml': 'yaml', '.yml': 'yaml',
  '.json': 'json',
  '.jsonc': 'jsonc',
  '.toml': 'toml',
  '.sql': 'sql',
  '.graphql': 'graphql', '.gql': 'graphql',
  '.proto': 'protobuf',
  '.tf': 'terraform', '.tfvars': 'terraform',
  '.html': 'html', '.htm': 'html',
  '.css': 'css', '.scss': 'css', '.sass': 'css', '.less': 'css',
  '.xml': 'xml',
  '.cfg': 'config', '.ini': 'config', '.env': 'config',
};
const BASENAME_TO_LANG = {
  'Dockerfile': 'dockerfile',
  'Makefile': 'makefile',
  'Jenkinsfile': 'jenkinsfile',
};

function detectLanguage(filePath) {
  const basename = path.basename(filePath);
  if (BASENAME_TO_LANG[basename]) return BASENAME_TO_LANG[basename];
  const ext = path.extname(filePath).toLowerCase();
  if (EXT_TO_LANG[ext]) return EXT_TO_LANG[ext];
  return ext ? ext.slice(1).toLowerCase() : 'unknown';
}

// Step 4: File category
function detectCategory(filePath) {
  const basename = path.basename(filePath);
  const ext = path.extname(filePath).toLowerCase();
  const lower = filePath.toLowerCase();

  // Infra first
  if (basename === 'Dockerfile' || basename.startsWith('docker-compose')) return 'infra';
  if (['.tf', '.tfvars'].includes(ext)) return 'infra';
  if (basename === 'Makefile' || basename === 'Jenkinsfile' || basename === 'Procfile' || basename === 'Vagrantfile') return 'infra';
  if (lower.includes('.github/workflows/') || lower.includes('.gitlab-ci') || lower.includes('.circleci/')) return 'infra';
  if (lower.includes('k8s/') || lower.includes('kubernetes/') || lower.endsWith('.k8s.yaml') || lower.endsWith('.k8s.yml')) return 'infra';

  // Docs
  if (['.md', '.rst', '.txt'].includes(ext)) return 'docs';

  // Config
  if (['.yaml', '.yml', '.json', '.jsonc', '.toml', '.xml', '.cfg', '.ini', '.env'].includes(ext)) return 'config';
  if (['tsconfig.json', 'package.json', 'pyproject.toml', 'Cargo.toml', 'go.mod'].includes(basename)) return 'config';

  // Data
  if (['.sql', '.graphql', '.gql', '.proto', '.prisma', '.csv'].includes(ext)) return 'data';
  if (basename.endsWith('.schema.json')) return 'data';

  // Script
  if (['.sh', '.bash', '.ps1', '.bat'].includes(ext)) return 'script';

  // Markup
  if (['.html', '.htm', '.css', '.scss', '.sass', '.less'].includes(ext)) return 'markup';

  return 'code';
}

// Step 5: Line counting
function countLines(filePaths) {
  const counts = {};
  const BATCH = 200;
  for (let i = 0; i < filePaths.length; i += BATCH) {
    const batch = filePaths.slice(i, i + BATCH);
    const absBatch = batch.map(f => path.join(projectRoot, f));
    try {
      const result = spawnSync('wc', ['-l', ...absBatch], { encoding: 'utf8', maxBuffer: 10 * 1024 * 1024 });
      if (result.stdout) {
        const lines = result.stdout.trim().split('\n');
        for (const line of lines) {
          const m = line.trim().match(/^(\d+)\s+(.+)$/);
          if (m) {
            const absPath = m[2].trim();
            const relPath = path.relative(projectRoot, absPath);
            counts[relPath] = parseInt(m[1], 10);
          }
        }
      }
    } catch (e) { /* ignore */ }
  }
  return counts;
}

const lineCounts = countLines(filteredFiles);

// Build file list
const files = filteredFiles.map(f => ({
  path: f,
  language: detectLanguage(f),
  sizeLines: lineCounts[f] || 0,
  fileCategory: detectCategory(f),
})).sort((a, b) => a.path.localeCompare(b.path));

// Step 6: Framework detection
const frameworks = [];
const detectedFrameworkSet = new Set();

function addFramework(name) {
  if (!detectedFrameworkSet.has(name)) {
    detectedFrameworkSet.add(name);
    frameworks.push(name);
  }
}

// package.json
const pkgJsonPath = path.join(projectRoot, 'admin', 'package.json');
if (fs.existsSync(pkgJsonPath)) {
  try {
    const pkg = JSON.parse(fs.readFileSync(pkgJsonPath, 'utf8'));
    const deps = { ...pkg.dependencies, ...pkg.devDependencies };
    const jsFrameworkMap = {
      'react': 'React', 'vue': 'Vue', 'svelte': 'Svelte', '@angular/core': 'Angular',
      'express': 'Express', 'fastify': 'Fastify', 'koa': 'Koa',
      'next': 'Next.js', 'nuxt': 'Nuxt', 'vite': 'Vite',
      'vitest': 'Vitest', 'jest': 'Jest', 'mocha': 'Mocha',
      'tailwindcss': 'Tailwind CSS', 'prisma': 'Prisma',
      'typeorm': 'TypeORM', 'sequelize': 'Sequelize', 'mongoose': 'Mongoose',
      'redux': 'Redux', 'zustand': 'Zustand', 'mobx': 'MobX',
      '@apollo/client': 'Apollo Client',
    };
    for (const [dep, fname] of Object.entries(jsFrameworkMap)) {
      if (deps[dep]) addFramework(fname);
    }
  } catch (e) {}
}

// Also check root package.json
const rootPkgPath = path.join(projectRoot, 'package.json');
if (fs.existsSync(rootPkgPath)) {
  try {
    const pkg = JSON.parse(fs.readFileSync(rootPkgPath, 'utf8'));
    const deps = { ...pkg.dependencies, ...pkg.devDependencies };
    const jsFrameworkMap = {
      'react': 'React', 'vue': 'Vue', 'svelte': 'Svelte', '@angular/core': 'Angular',
      'express': 'Express', 'fastify': 'Fastify', 'koa': 'Koa',
      'next': 'Next.js', 'nuxt': 'Nuxt', 'vite': 'Vite',
      'vitest': 'Vitest', 'jest': 'Jest', 'mocha': 'Mocha',
      'tailwindcss': 'Tailwind CSS', 'prisma': 'Prisma',
    };
    for (const [dep, fname] of Object.entries(jsFrameworkMap)) {
      if (deps[dep]) addFramework(fname);
    }
  } catch (e) {}
}

// go.mod
const goModPath = path.join(projectRoot, 'controller', 'go.mod');
if (fs.existsSync(goModPath)) {
  const goModContent = fs.readFileSync(goModPath, 'utf8');
  const goFrameworks = {
    'github.com/gin-gonic/gin': 'Gin', 'github.com/labstack/echo': 'Echo',
    'github.com/gofiber/fiber': 'Fiber', 'github.com/go-chi/chi': 'Chi',
    'gorm.io/gorm': 'GORM', 'github.com/99designs/gqlgen': 'gqlgen',
    'github.com/jackc/pgx': 'pgx (PostgreSQL)',
  };
  for (const [mod, fname] of Object.entries(goFrameworks)) {
    if (goModContent.includes(mod)) addFramework(fname);
  }
}

// Cargo.toml files
for (const cargoPath of ['connector/Cargo.toml', 'shield/Cargo.toml', 'client/Cargo.toml', 'Cargo.toml'].map(p => path.join(projectRoot, p))) {
  if (fs.existsSync(cargoPath)) {
    const content = fs.readFileSync(cargoPath, 'utf8');
    const rustFrameworks = {
      'actix-web': 'Actix Web', 'axum': 'Axum', 'rocket': 'Rocket',
      'diesel': 'Diesel', 'tokio': 'Tokio', 'serde': 'Serde', 'warp': 'Warp',
      'tonic': 'Tonic (gRPC)',
    };
    for (const [crate, fname] of Object.entries(rustFrameworks)) {
      if (content.includes(crate)) addFramework(fname);
    }
  }
}

// Infrastructure
const hasDockerfile = filteredFiles.some(f => path.basename(f) === 'Dockerfile');
if (hasDockerfile) addFramework('Docker');
const hasDockerCompose = filteredFiles.some(f => path.basename(f).startsWith('docker-compose'));
if (hasDockerCompose) addFramework('Docker Compose');
const hasTf = filteredFiles.some(f => f.endsWith('.tf'));
if (hasTf) addFramework('Terraform');
const hasGhActions = filteredFiles.some(f => f.includes('.github/workflows/'));
if (hasGhActions) addFramework('GitHub Actions');
const hasGitLabCI = filteredFiles.some(f => path.basename(f) === '.gitlab-ci.yml');
if (hasGitLabCI) addFramework('GitLab CI');

// Step 7: Complexity
const total = files.length;
let estimatedComplexity = 'small';
if (total > 500) estimatedComplexity = 'very-large';
else if (total > 150) estimatedComplexity = 'large';
else if (total > 30) estimatedComplexity = 'moderate';

// Step 8: Project name
let projectName = path.basename(projectRoot);
if (fs.existsSync(rootPkgPath)) {
  try {
    const pkg = JSON.parse(fs.readFileSync(rootPkgPath, 'utf8'));
    if (pkg.name) projectName = pkg.name;
  } catch (e) {}
}
// Use "zecurity" since that's the project
projectName = 'zecurity';

// README head
let readmeHead = '';
for (const rname of ['README.md', 'readme.md', 'Readme.md']) {
  const rpath = path.join(projectRoot, rname);
  if (fs.existsSync(rpath)) {
    const lines = fs.readFileSync(rpath, 'utf8').split('\n').slice(0, 10).join('\n');
    readmeHead = lines;
    break;
  }
}

// Raw description
let rawDescription = '';
if (fs.existsSync(rootPkgPath)) {
  try {
    const pkg = JSON.parse(fs.readFileSync(rootPkgPath, 'utf8'));
    if (pkg.description) rawDescription = pkg.description;
  } catch (e) {}
}

// Languages
const langSet = new Set(files.map(f => f.language));
const languages = [...langSet].sort();

// Step 9: Import Map
function extractImports(filePath, content, lang) {
  const imports = [];
  if (lang === 'typescript' || lang === 'javascript') {
    const patterns = [
      /import\s+.*?from\s+['"]([^'"]+)['"]/g,
      /require\s*\(\s*['"]([^'"]+)['"]\s*\)/g,
    ];
    for (const pat of patterns) {
      let m;
      while ((m = pat.exec(content)) !== null) {
        if (m[1].startsWith('.')) imports.push(m[1]);
      }
    }
  } else if (lang === 'python') {
    const relPat = /from\s+(\.+\w*(?:\.\w+)*)\s+import/g;
    let m;
    while ((m = relPat.exec(content)) !== null) imports.push(m[1]);
    const absPat = /^(?:from|import)\s+([\w.]+)/gm;
    while ((m = absPat.exec(content)) !== null) {
      if (!m[1].startsWith('.')) imports.push(m[1]);
    }
  } else if (lang === 'go') {
    const pat = /import\s+(?:\(\s*([\s\S]*?)\s*\)|"([^"]+)")/g;
    let m;
    while ((m = pat.exec(content)) !== null) {
      const block = m[1] || m[2];
      if (block) {
        const pathPat = /"([^"]+)"/g;
        let pm;
        while ((pm = pathPat.exec(block)) !== null) imports.push(pm[1]);
      }
    }
  } else if (lang === 'rust') {
    const pat = /(?:use\s+(?:crate|super)::|mod\s+(\w+))/g;
    let m;
    while ((m = pat.exec(content)) !== null) if (m[1]) imports.push(m[1]);
  }
  return imports;
}

const fileSet = new Set(filteredFiles);

function resolveImport(importPath, importerPath, lang, fileSet) {
  const importerDir = path.dirname(importerPath);

  if (lang === 'typescript' || lang === 'javascript') {
    if (!importPath.startsWith('.')) return null;
    const resolved = path.normalize(path.join(importerDir, importPath));
    const exts = ['.ts', '.tsx', '.js', '.jsx', '/index.ts', '/index.tsx', '/index.js', '/index.jsx'];
    if (fileSet.has(resolved)) return resolved;
    for (const ext of exts) {
      const candidate = resolved + ext;
      if (fileSet.has(candidate)) return candidate;
    }
    // Try without extension if already has one
    for (const ext of ['.ts', '.tsx', '.js', '.jsx']) {
      if (resolved.endsWith(ext) && fileSet.has(resolved)) return resolved;
    }
  } else if (lang === 'python') {
    if (importPath.startsWith('.')) {
      // relative
      const dots = importPath.match(/^\.+/)[0].length;
      let base = importerDir;
      for (let i = 1; i < dots; i++) base = path.dirname(base);
      const rest = importPath.replace(/^\.+/, '').replace(/\./g, '/');
      const candidate = rest ? path.join(base, rest + '.py') : null;
      if (candidate && fileSet.has(candidate)) return candidate;
    } else {
      const modPath = importPath.replace(/\./g, '/');
      for (const suffix of ['.py', '/__init__.py']) {
        const candidate = modPath + suffix;
        if (fileSet.has(candidate)) return candidate;
      }
    }
  }
  return null;
}

const importMap = {};
for (const file of files) {
  if (file.fileCategory !== 'code') {
    importMap[file.path] = [];
    continue;
  }

  const absPath = path.join(projectRoot, file.path);
  let content = '';
  try {
    content = fs.readFileSync(absPath, 'utf8');
  } catch (e) {
    importMap[file.path] = [];
    continue;
  }

  const rawImports = extractImports(file.path, content, file.language);
  const resolved = new Set();
  for (const imp of rawImports) {
    const r = resolveImport(imp, file.path, file.language, fileSet);
    if (r) resolved.add(r);
  }
  importMap[file.path] = [...resolved];
}

// Write output
const output = {
  scriptCompleted: true,
  name: projectName,
  rawDescription,
  readmeHead,
  languages,
  frameworks,
  files,
  totalFiles: files.length,
  filteredByIgnore,
  estimatedComplexity,
  importMap,
};

fs.writeFileSync(outputFile, JSON.stringify(output, null, 2));
process.exit(0);
