function check(a,b){if(a!==b)throw a+" !== "+b;} check(NaN!==NaN,true);var d=false;NaN=5;check(isNaN(NaN),true);console.log('PASS');
