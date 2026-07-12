"use strict";function check(a,b){if(a!==b)throw a+" !== "+b;} eval('var evalLocal=1;');check(typeof evalLocal,'undefined');console.log('PASS');
