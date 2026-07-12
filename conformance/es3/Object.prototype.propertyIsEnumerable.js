function check(a,b){if(a!==b)throw a+" !== "+b;} var o={a:1};check(o.propertyIsEnumerable('a'),true);check(o.propertyIsEnumerable('b'),false);console.log('PASS');
